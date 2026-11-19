package api

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// L3：直连下载的链接不绑定用户，撤销成员的权限时面板给各节点换密钥，让已经签出的链接立即失效：
// 删除成员、降低角色、改访问范围、重置密码、退出登录、撤销登录设备时换；提升角色、没登录的退出请求不换。
func TestRevokeRotatesDirectKeys(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.New(slog.DiscardHandler), h.a.Sealer, nil)
	var rotations atomic.Int32
	old := rotateDirect
	rotateDirect = func(*nodes.Registry) { rotations.Add(1) }
	t.Cleanup(func() { rotateDirect = old })

	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	bob, err := h.a.Auth.CreateUser(ctx, "bob", "bob-password-1", auth.ViewerRoleID)
	if err != nil {
		t.Fatal(err)
	}
	carol, err := h.a.Auth.CreateUser(ctx, "carol", "carol-password-1", auth.OperatorRoleID)
	if err != nil {
		t.Fatal(err)
	}
	_, sc := h.loginElevated("admin", "admin-password-1")

	// expect 做一件事，等后台换完，检查换了没有。
	expect := func(name string, want bool, do func() error) {
		t.Helper()
		before := rotations.Load()
		if err := do(); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for want && rotations.Load() == before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !want {
			time.Sleep(100 * time.Millisecond)
		}
		if got := rotations.Load() != before; got != want {
			t.Errorf("%s：换了密钥 %v，应为 %v", name, got, want)
		}
	}
	update := func(req *nagiv1.UpdateUserRequest) func() error {
		return func() error {
			_, err := sc.UpdateUser(ctx, connect.NewRequest(req))
			return err
		}
	}

	expect("提升角色", false, update(&nagiv1.UpdateUserRequest{Id: bob, Role: auth.RoleOperator}))
	expect("降低角色", true, update(&nagiv1.UpdateUserRequest{Id: bob, Role: auth.RoleViewer}))
	expect("修改访问范围", true, update(&nagiv1.UpdateUserRequest{Id: bob, Scope: &nagiv1.UserScope{AllNodes: true}}))
	// 重置密码要先用密码做高权限确认。
	if _, err := h.db.ExecContext(ctx, `UPDATE sessions SET elevated_until = ?, elevated_by = 'password'`, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	expect("重置密码", true, update(&nagiv1.UpdateUserRequest{Id: bob, Password: "bob-new-password-2"}))

	// 自己撤销另一个登录设备。
	_, first := h.login("carol", "carol-password-1")
	h.login("carol", "carol-password-1")
	sessions, err := first.ListSessions(ctx, connect.NewRequest(&nagiv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var other string
	for _, s := range sessions.Msg.GetSessions() {
		if !s.GetCurrent() {
			other = s.GetId()
		}
	}
	expect("撤销登录设备", true, func() error {
		_, err := first.RevokeSession(ctx, connect.NewRequest(&nagiv1.RevokeSessionRequest{Id: other}))
		return err
	})

	ac, _ := h.login("carol", "carol-password-1")
	expect("退出登录", true, func() error {
		_, err := ac.Logout(ctx, connect.NewRequest(&nagiv1.LogoutRequest{}))
		return err
	})
	anon, _ := h.client()
	expect("没登录的退出请求", false, func() error {
		_, err := anon.Logout(ctx, connect.NewRequest(&nagiv1.LogoutRequest{}))
		return err
	})

	expect("删除成员", true, func() error {
		_, err := sc.DeleteUser(ctx, connect.NewRequest(&nagiv1.DeleteUserRequest{Id: carol}))
		return err
	})
}
