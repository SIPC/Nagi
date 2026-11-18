package api

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 角色的新建、修改、删除和改成员的角色：接口的规则，都记进审计（见 docs/panel.md#成员和角色）。
func TestRolesAPI(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	_, sc := h.login("admin", "admin-password-1")

	// 新建：前提的权限自动补上，只给管理员的权限不能勾。
	r, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "看日志", Permissions: []string{"project.logs"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Msg.GetPermissions(); !slices.Equal(got, []string{"project.view", "project.logs"}) {
		t.Errorf("权限：%v", got)
	}
	if _, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "审计", Permissions: []string{"panel.audit"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("审计日志只给管理员：%v", err)
	}
	if _, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "看日志", Permissions: []string{"project.view"}})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("名称重复：%v", err)
	}
	if _, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "x", Permissions: []string{"project.nope"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("不存在的权限：%v", err)
	}
	// 管理员角色不能改；预置角色可以改，不能删。
	if _, err := sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.AdminRoleID, Permissions: []string{"project.view"}})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("管理员角色：%v", err)
	}
	if _, err := sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.ViewerRoleID, Permissions: []string{"project.view", "node.view"}})); err != nil {
		t.Errorf("预置角色可以编辑：%v", err)
	}
	if _, err := sc.DeleteRole(ctx, connect.NewRequest(&nagiv1.DeleteRoleRequest{Id: auth.ViewerRoleID})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("预置角色不能删除：%v", err)
	}
	// 成员用自定义角色；正在使用的角色不能删。
	u, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "bob", Password: "bob-password-1", RoleId: r.Msg.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Msg.GetScope().GetNodesFollow() != true || u.Msg.GetScope().GetAllNodes() || !u.Msg.GetScope().GetAllProjects() {
		t.Errorf("新成员的默认范围是项目全部、节点跟随项目：%v", u.Msg.GetScope())
	}
	if _, err := sc.DeleteRole(ctx, connect.NewRequest(&nagiv1.DeleteRoleRequest{Id: r.Msg.GetId()})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("正在使用的角色不能删除：%v", err)
	}
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: u.Msg.GetId(), RoleId: auth.OperatorRoleID})); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.DeleteRole(ctx, connect.NewRequest(&nagiv1.DeleteRoleRequest{Id: r.Msg.GetId()})); err != nil {
		t.Errorf("没人用了可以删除：%v", err)
	}
	users, err := sc.ListUsers(ctx, connect.NewRequest(&nagiv1.ListUsersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range users.Msg.GetUsers() {
		if x.GetUsername() == "bob" && (x.GetRoleId() != auth.OperatorRoleID || x.GetRoleBuiltin() != auth.RoleOperator || x.GetRole() != auth.RoleOperator) {
			t.Errorf("成员的角色：%v", x)
		}
	}
	// 都记进审计。
	got := actions(h.list(sc, nil))
	for _, want := range []string{"CreateRole", "UpdateRole", "DeleteRole", "UpdateUser"} {
		if !strings.Contains(got, want) {
			t.Errorf("审计里缺少 %s：%s", want, got)
		}
	}
}

// 有「成员」权限的非管理员不能越过自己：只能授予自己有的权限，不能管理管理员，范围不超过自己的。
func TestMemberManagerLimits(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	_, admin := h.login("admin", "admin-password-1")
	mgr, err := admin.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "管成员", Permissions: []string{"panel.members", "project.view", "project.logs"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "mgr", Password: "mgr-password-1", RoleId: mgr.Msg.GetId(),
		Scope: &nagiv1.UserScope{AllProjects: true, NodesFollow: true}})); err != nil {
		t.Fatal(err)
	}
	_, sc := h.login("mgr", "mgr-password-1")
	if _, err := sc.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "多", Permissions: []string{"project.deploy"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("不能授予自己没有的权限：%v", err)
	}
	if _, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "op", Password: "op-password-1", RoleId: auth.OperatorRoleID})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("不能给别人比自己多的角色：%v", err)
	}
	if _, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "ad", Password: "ad-password-1", RoleId: auth.AdminRoleID})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("不能新建管理员：%v", err)
	}
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: 1, Password: "new-password-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("不能重置管理员的密码：%v", err)
	}
	if _, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "v", Password: "v-password-1", RoleId: mgr.Msg.GetId(),
		Scope: &nagiv1.UserScope{AllProjects: true, AllNodes: true}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("范围不能超过自己的（自己是跟随项目）：%v", err)
	}
	if _, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "v", Password: "v-password-1", RoleId: mgr.Msg.GetId()})); err != nil {
		t.Errorf("同样的权限、默认范围可以：%v", err)
	}
	roles, err := sc.ListRoles(ctx, connect.NewRequest(&nagiv1.ListRolesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := roles.Msg.GetGrantable(); !slices.Equal(got, []string{"project.view", "project.logs", "panel.members"}) {
		t.Errorf("能授予的权限：%v", got)
	}
	// 成员看不到面板的设置和审计。
	if _, err := sc.GetAlertConfig(ctx, connect.NewRequest(&nagiv1.GetAlertConfigRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("没有面板设置：%v", err)
	}
	if _, err := sc.ListActivity(ctx, connect.NewRequest(&nagiv1.ListActivityRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("没有审计日志：%v", err)
	}
}
