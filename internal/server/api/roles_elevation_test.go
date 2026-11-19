package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// elevationDenied：接口因为没做（或者做的方式不算数的）高权限确认而拒绝。
func elevationDenied(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce) && ce.Code() == connect.CodePermissionDenied && ce.Meta().Get(elevationHeader) == "required"
}

// 新建、修改角色和成员一样只认密码或验证码做的高权限确认：没确认、只在手机上确认过的会话不能给成员在用的角色加权限。
func TestRoleChangesNeedPasswordElevation(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	_, sc := h.login("admin", "admin-password-1")
	elevate := func(by string) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, `UPDATE sessions SET elevated_until = ?, elevated_by = ?`, time.Now().Add(time.Hour).Unix(), by); err != nil {
			t.Fatal(err)
		}
	}
	calls := []struct {
		name string
		call func() error
	}{
		{"新建角色", func() error {
			_, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "运维", Permissions: []string{"node.root"}}))
			return err
		}},
		{"给预置操作员加权限", func() error {
			_, err := sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.OperatorRoleID,
				Permissions: []string{"node.root", "panel.members", "project.privileged"}}))
			return err
		}},
		{"只改名称", func() error {
			_, err := sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.OperatorRoleID, Name: "值班", KeepPermissions: true}))
			return err
		}},
	}
	for _, by := range []string{"", auth.ElevatedByApp} {
		if by != "" {
			elevate(by)
		}
		for _, c := range calls {
			if err := c.call(); !elevationDenied(err) {
				t.Errorf("确认方式 %q，%s：应该要求验证身份，得到 %v", by, c.name, err)
			}
		}
	}
	if r, err := h.a.Q.GetRole(ctx, auth.OperatorRoleID); err != nil || rolePerms(r)&auth.PermsOf(auth.PermNodeRoot) != 0 {
		t.Errorf("没确认时预置操作员不应该被改动：%v %v", r.Permissions, err)
	}
	elevate(auth.ElevatedByPassword)
	for _, c := range calls {
		if err := c.call(); err != nil {
			t.Errorf("用密码确认后，%s：%v", c.name, err)
		}
	}
}

// 删除成员、删除角色要先做高权限确认，手机上的确认也算。
func TestDeleteMemberAndRoleNeedElevation(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	bob, err := h.a.Auth.CreateUser(ctx, "bob", "bob-password-1", auth.AdminRoleID)
	if err != nil {
		t.Fatal(err)
	}
	role, err := h.a.Q.CreateRole(ctx, "临时", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	_, sc := h.login("admin", "admin-password-1")
	if _, err := sc.DeleteUser(ctx, connect.NewRequest(&nagiv1.DeleteUserRequest{Id: bob})); !elevationDenied(err) {
		t.Errorf("没确认时删除成员：应该要求验证身份，得到 %v", err)
	}
	if _, err := sc.DeleteRole(ctx, connect.NewRequest(&nagiv1.DeleteRoleRequest{Id: role})); !elevationDenied(err) {
		t.Errorf("没确认时删除角色：应该要求验证身份，得到 %v", err)
	}
	if _, err := h.a.Q.GetUserByID(ctx, bob); err != nil {
		t.Errorf("没确认时成员不应该被删掉：%v", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE sessions SET elevated_until = ?, elevated_by = ?`, time.Now().Add(time.Hour).Unix(), auth.ElevatedByApp); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.DeleteUser(ctx, connect.NewRequest(&nagiv1.DeleteUserRequest{Id: bob})); err != nil {
		t.Errorf("在手机上确认后删除成员：%v", err)
	}
	if _, err := sc.DeleteRole(ctx, connect.NewRequest(&nagiv1.DeleteRoleRequest{Id: role})); err != nil {
		t.Errorf("在手机上确认后删除角色：%v", err)
	}
}

// 只改名称（keep_permissions）不动角色的权限；不带它时 permissions 为空仍然是清空，和以前一样。
func TestUpdateRoleKeepPermissions(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	_, sc := h.loginElevated("admin", "admin-password-1")
	r, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "看日志", Permissions: []string{"project.logs"}}))
	if err != nil {
		t.Fatal(err)
	}
	want := r.Msg.GetPermissions()
	// 只改名称：请求里带着别的权限也不看。
	got, err := sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: r.Msg.GetId(), Name: "日志", KeepPermissions: true, Permissions: []string{"node.root"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetName() != "日志" || !slices.Equal(got.Msg.GetPermissions(), want) {
		t.Errorf("只改名称后：%q %v，权限应为 %v", got.Msg.GetName(), got.Msg.GetPermissions(), want)
	}
	// 名称也不填：什么都不改。
	got, err = sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: r.Msg.GetId(), KeepPermissions: true}))
	if err != nil || got.Msg.GetName() != "日志" || !slices.Equal(got.Msg.GetPermissions(), want) {
		t.Errorf("什么都不改：%v %v", got, err)
	}
	// 带权限的调用照常整个替换。
	got, err = sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: r.Msg.GetId(), Name: "日志", Permissions: []string{"project.view", "node.view"}}))
	if err != nil || !slices.Equal(got.Msg.GetPermissions(), []string{"project.view", "node.view"}) {
		t.Errorf("替换权限：%v %v", got, err)
	}
	got, err = sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: r.Msg.GetId()}))
	if err != nil || len(got.Msg.GetPermissions()) != 0 {
		t.Errorf("不带 keep_permissions 时 permissions 为空是清空：%v %v", got, err)
	}
}
