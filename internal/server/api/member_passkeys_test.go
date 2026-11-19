package api

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// M5：攻击者给成员加了通行密钥以后，管理员重置密码、清除两步验证时一并删除；
// 管理员能在成员列表里看到通行密钥，也能单独清除。
func TestMemberPasskeysCleared(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	bob, err := h.a.Auth.CreateUser(ctx, "bob", "bob-password-1", auth.OperatorRoleID)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) {
		if _, err := h.a.Q.AddPasskey(ctx, store.Passkey{UserID: bob, CredentialID: []byte(name), PublicKey: []byte("k"), Name: name, CreatedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(sc interface {
		ListUsers(context.Context, *connect.Request[nagiv1.ListUsersRequest]) (*connect.Response[nagiv1.ListUsersResponse], error)
	}) int {
		res, err := sc.ListUsers(ctx, connect.NewRequest(&nagiv1.ListUsersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range res.Msg.GetUsers() {
			if u.GetId() == bob {
				return len(u.GetPasskeys())
			}
		}
		t.Fatal("没有 bob")
		return 0
	}
	ac, sc := h.login("admin", "admin-password-1")
	// 重置密码、清除两步验证和通行密钥要先用密码做高权限确认。
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: bob, ClearPasskeys: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("没有确认时应该拒绝：%v", err)
	}
	if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: "admin-password-1"})); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		req  *nagiv1.UpdateUserRequest
	}{
		{"重置密码", &nagiv1.UpdateUserRequest{Id: bob, Password: "new-password-42"}},
		{"清除两步验证", &nagiv1.UpdateUserRequest{Id: bob, ClearTotp: true}},
		{"清除通行密钥", &nagiv1.UpdateUserRequest{Id: bob, ClearPasskeys: true}},
	} {
		add(c.name + "-1")
		add(c.name + "-2")
		if n := count(sc); n != 2 {
			t.Fatalf("%s 之前管理员应该看到 2 个通行密钥，实际 %d", c.name, n)
		}
		if _, err := sc.UpdateUser(ctx, connect.NewRequest(c.req)); err != nil {
			t.Fatal(err)
		}
		if n := count(sc); n != 0 {
			t.Errorf("%s 以后还剩 %d 个通行密钥", c.name, n)
		}
	}
	// 只改角色不动通行密钥。
	add("keep")
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: bob, Role: auth.RoleViewer})); err != nil {
		t.Fatal(err)
	}
	if n := count(sc); n != 1 {
		t.Errorf("只改角色不应删除通行密钥，实际剩 %d 个", n)
	}
}
