package api

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
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

// 审查 2026-11-19-87c02b5 的 #2、#3、#6、#11、#16：范围小的成员管不了范围大的成员和他们在用的角色；跟随项目的成员
// 不能授予节点标签；自己的登录凭据不能在这里改；成员列表不带出范围外的 ID；角色名称不能冒充预置角色。
func TestMemberManagerScope(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	_, admin := h.login("admin", "admin-password-1")
	mgrRole, err := admin.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "管成员", Permissions: []string{"panel.members", "project.view", "project.logs"}}))
	if err != nil {
		t.Fatal(err)
	}
	logRole, err := admin.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: "看日志", Permissions: []string{"project.view", "project.logs"}}))
	if err != nil {
		t.Fatal(err)
	}
	// mgr：项目指定（一个都没有）、节点跟随项目。big：权限不比 mgr 多，范围是全部。
	if _, err := admin.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "mgr", Password: "mgr-password-1", RoleId: mgrRole.Msg.GetId(),
		Scope: &nagiv1.UserScope{NodesFollow: true}})); err != nil {
		t.Fatal(err)
	}
	big, err := admin.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "big", Password: "big-password-1", RoleId: logRole.Msg.GetId(),
		Scope: &nagiv1.UserScope{AllProjects: true, AllNodes: true}}))
	if err != nil {
		t.Fatal(err)
	}
	ac, sc := h.login("mgr", "mgr-password-1")
	if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: "mgr-password-1"})); err != nil {
		t.Fatal(err)
	}
	denied := func(what string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s：%v，想要 permission_denied", what, err)
		}
	}
	_, err = sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: big.Msg.GetId(), RoleId: mgrRole.Msg.GetId(), Password: "new-password-1", ClearTotp: true}))
	denied("接管范围更大的成员", err)
	_, err = sc.DeleteUser(ctx, connect.NewRequest(&nagiv1.DeleteUserRequest{Id: big.Msg.GetId()}))
	denied("删除范围更大的成员", err)
	_, err = sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: logRole.Msg.GetId(), Permissions: []string{"project.view", "project.logs", "panel.members"}}))
	denied("改范围更大的成员在用的角色", err)
	_, err = sc.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.ViewerRoleID, Permissions: []string{"project.view", "project.logs"}}))
	denied("改预置角色", err)
	// 跟随项目的成员不能授予节点标签（给自己也不行）。
	me, err := h.a.Q.GetUserByUsername(ctx, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	_, err = sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: me.ID, Scope: &nagiv1.UserScope{NodeTags: []string{"prod"}}}))
	denied("跟随项目的成员给自己加节点标签", err)
	_, err = sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "x", Password: "x-password-1", RoleId: logRole.Msg.GetId(),
		Scope: &nagiv1.UserScope{NodeTags: []string{"prod"}}}))
	denied("跟随项目的成员给别人节点标签", err)
	// 不存在的项目和范围外的项目报同一个错。
	_, err = sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "x", Password: "x-password-1", RoleId: logRole.Msg.GetId(),
		Scope: &nagiv1.UserScope{ProjectIds: []string{"nope"}, NodesFollow: true}}))
	denied("不存在的项目", err)
	// 自己的密码、两步验证不能在成员管理里改。
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: me.ID, Password: "new-password-1", ClearTotp: true})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("在成员管理里改自己的密码：%v", err)
	}
	// 成员列表：范围外的分组、标签、ID 不给。
	if _, err := admin.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: big.Msg.GetId(),
		Scope: &nagiv1.UserScope{ProjectGroups: []string{"secret"}, NodeTags: []string{"prod"}}})); err != nil {
		t.Fatal(err)
	}
	list, err := sc.ListUsers(ctx, connect.NewRequest(&nagiv1.ListUsersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Msg.GetUsers() {
		if u.GetUsername() == "big" && (len(u.GetScope().GetProjectGroups()) != 0 || len(u.GetScope().GetNodeTags()) != 0) {
			t.Errorf("成员列表带出了范围外的分组和标签：%v", u.GetScope())
		}
	}
	// 角色名称：不能叫预置角色的名字，不能带看不见的字符。
	for _, name := range []string{"管理员", "Admin", "只读", "看\u200b日志"} {
		if _, err := admin.CreateRole(ctx, connect.NewRequest(&nagiv1.CreateRoleRequest{Name: name, Permissions: []string{"project.view"}})); err == nil {
			t.Errorf("角色名称 %q 应该拒绝", name)
		}
	}
	if _, err := admin.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.OperatorRoleID, Name: "只读", Permissions: []string{"project.view"}})); err == nil {
		t.Error("预置操作员不能改名成只读")
	}
	if _, err := admin.UpdateRole(ctx, connect.NewRequest(&nagiv1.UpdateRoleRequest{Id: auth.OperatorRoleID, Name: "操作员", Permissions: []string{"project.view"}})); err != nil {
		t.Errorf("预置角色可以用自己的名字：%v", err)
	}
}

// 最后一个管理员的检查和写入之间不能插进别的修改（2026-11-19-87c02b5#8）：两个管理员同时把对方降级，至少留下一个。
func TestLastAdminRace(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	a1, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := h.a.Auth.CreateUser(ctx, "admin2", "admin-password-2", auth.AdminRoleID)
	if err != nil {
		t.Fatal(err)
	}
	_, s1 := h.login("admin", "admin-password-1")
	_, s2 := h.login("admin2", "admin-password-2")
	for i := 0; i < 5; i++ {
		done := make(chan struct{}, 2)
		go func() {
			_, _ = s1.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: a2, RoleId: auth.ViewerRoleID}))
			done <- struct{}{}
		}()
		go func() {
			_, _ = s2.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: a1, RoleId: auth.ViewerRoleID}))
			done <- struct{}{}
		}()
		<-done
		<-done
		n, err := h.a.Q.CountAdmins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n < 1 {
			t.Fatal("面板没有管理员了")
		}
		// 恢复成两个管理员再试。
		for _, id := range []int64{a1, a2} {
			if err := h.a.Q.SetUserRole(ctx, store.SetUserRoleParams{Role: auth.UsersRole(auth.AdminRoleID), RoleID: auth.AdminRoleID, ID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
