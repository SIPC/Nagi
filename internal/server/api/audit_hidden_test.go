package api

import (
	"context"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 文件操作的记录，看不到路径的成员显示时只留下容器，搜索也只按容器匹配：否则按有没有结果能一段一段拼出路径。
func TestAuditSearchHiddenFilePaths(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	for _, u := range []struct{ name, role string }{{"admin", auth.RoleAdmin}, {"viewer", auth.RoleViewer}, {"op", auth.RoleOperator}} {
		if _, err := h.a.Auth.CreateUser(ctx, u.name, u.name+"-password-1", auth.BuiltinRoleID(u.role)); err != nil {
			t.Fatal(err)
		}
	}
	actx := auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin})
	h.a.Record(actx, audit.Event{Action: "DownloadVolumeFile", ProjectID: "p1", TargetType: "file", TargetID: "/data/payroll-2026.xlsx",
		TargetName: "web:/data/payroll-2026.xlsx"})
	h.a.Record(actx, audit.Event{Action: "DeleteVolumeFile", ProjectID: "p1", TargetType: "file", TargetID: "/data/old-payroll.csv",
		TargetName: "web:/data/old-payroll.csv"})
	// 不在容器里的文件，对象名称就是路径。
	h.a.Record(actx, audit.Event{Action: "DownloadVolumeFile", ProjectID: "p1", TargetType: "file", TargetID: "/srv/payroll.bak", TargetName: "/srv/payroll.bak"})

	_, vc := h.login("viewer", "viewer-password-1")
	_, oc := h.login("op", "op-password-1")
	_, ac := h.login("admin", "admin-password-1")
	n := func(c nagiv1connect.SettingsServiceClient, q string) int {
		return len(h.list(c, &nagiv1.AuditFilter{Search: q}))
	}
	check := func(who string, c nagiv1connect.SettingsServiceClient, q string, want int) {
		t.Helper()
		if got := n(c, q); got != want {
			t.Errorf("%s「%s」：%d 条，应该是 %d 条", who, q, got, want)
		}
	}
	check("只读成员按路径", vc, "payroll", 0)
	check("只读成员按路径的片段", vc, "payroll-2026.x", 0)
	check("只读成员按容器", vc, "web", 2)
	check("只读成员按冒号", vc, ":", 0)
	// 预置的操作员有「文件查看」「文件修改」，看得到路径。
	check("操作员按路径", oc, "payroll", 3)
	check("管理员按路径", ac, "payroll", 3)

	// 有「查看配置」、只有「文件查看」：下载的路径看得到，删除的看不到。
	if err := h.a.Q.SetRolePermissions(ctx, auth.ViewerRoleID, []string{"project.view", "project.config", "project.files.read"}); err != nil {
		t.Fatal(err)
	}
	check("只能查看文件的成员按路径", vc, "payroll", 2)
	check("只能查看文件的成员按删除的路径", vc, "old-payroll", 0)
	check("只能查看文件的成员按容器", vc, "web", 2)
	for _, e := range h.list(vc, &nagiv1.AuditFilter{Search: "web"}) {
		if e.GetAction() == "DeleteVolumeFile" && e.GetTargetName() != "web" {
			t.Errorf("看不到路径时对象应该只留容器：%q", e.GetTargetName())
		}
	}
}

// 项目设置的记录：有「项目管理」就看得到参数，其中出站限制和内部端口、告警规则、快照几项另要相应的权限，没有的去掉；
// 去掉了内容时失败详情也不给。MCP 的 update_project 一样。
func TestAuditProjectSettingsParams(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	for _, u := range []struct{ name, role string }{{"admin", auth.RoleAdmin}, {"op", auth.RoleOperator}} {
		if _, err := h.a.Auth.CreateUser(ctx, u.name, u.name+"-password-1", auth.BuiltinRoleID(u.role)); err != nil {
			t.Fatal(err)
		}
	}
	// 预置的操作员有「快照」，没有「网络和域名」「告警规则」：加上「项目管理」。
	if err := h.a.Q.SetRolePermissions(ctx, auth.OperatorRoleID, append(auth.PresetOperator.Names(), "project.manage")); err != nil {
		t.Fatal(err)
	}
	actx := auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin})
	req := &nagiv1.UpdateProjectSettingsRequest{ProjectId: "p1", Name: "renamed", Settings: &nagiv1.ProjectSettings{
		Egress: &nagiv1.Egress{Mode: "block", Allow: []string{"192.168.1.20:5432"}}, InternalPorts: []string{"db:6543"}, SnapshotLimit: 7,
		AlertRules: &nagiv1.AlertRules{},
	}}
	h.a.Record(actx, audit.Event{Action: "UpdateProjectSettings", ProjectID: "p1", TargetType: "project", TargetID: "p1", TargetName: "a",
		Params: audit.Params(req), Result: audit.ResultFailed, Error: "出站放行地址 192.168.1.20:5432 无效"})
	h.a.Record(actx, audit.Event{Category: audit.MCP, Action: "mcp:update_project", ProjectID: "p1", TargetType: "tool", TargetID: "update_project",
		TargetName: "update_project",
		Params:     `{"perms":["project.view","project.manage"],"arguments":{"project":"a","name":"renamed","egress_allow":["192.168.1.20:5432"],"internal_ports":["db:6543"],"snapshot_limit":7}}`})

	_, oc := h.login("op", "op-password-1")
	_, ac := h.login("admin", "admin-password-1")
	byAction := func(c nagiv1connect.SettingsServiceClient) map[string]*nagiv1.AuditEvent {
		out := map[string]*nagiv1.AuditEvent{}
		for _, e := range h.list(c, &nagiv1.AuditFilter{}) {
			out[e.GetAction()] = e
		}
		return out
	}
	op := byAction(oc)
	for _, action := range []string{"UpdateProjectSettings", "mcp:update_project"} {
		e := op[action]
		if e == nil {
			t.Fatalf("操作员看不到 %s 的记录", action)
		}
		p := e.GetParams()
		for _, hidden := range []string{"192.168.1.20", "6543", "egress", "internal_ports", "alert_rules"} {
			if strings.Contains(p, hidden) {
				t.Errorf("没有「网络和域名」「告警规则」时 %s 的参数不该有 %q：%s", action, hidden, p)
			}
		}
		for _, shown := range []string{"renamed", "snapshot_limit"} {
			if !strings.Contains(p, shown) {
				t.Errorf("%s 的参数应该留着 %q：%s", action, shown, p)
			}
		}
		if e.GetError() != "" {
			t.Errorf("%s：去掉了参数时不该给失败详情：%q", action, e.GetError())
		}
	}
	adm := byAction(ac)
	if p := adm["UpdateProjectSettings"].GetParams(); !strings.Contains(p, "192.168.1.20:5432") || !strings.Contains(p, "db:6543") {
		t.Errorf("管理员看得到全部参数：%s", p)
	}
	if adm["UpdateProjectSettings"].GetError() == "" {
		t.Error("管理员看得到失败详情")
	}

	// 有全部相应权限时原样给；截断过的参数读不出来，整个不给。
	full := &access{perms: auth.PresetOperator | auth.PermsOf(auth.PermProjectManage, auth.PermNetwork, auth.PermAlerts)}
	if p, hid := hideSettingsParams("UpdateProjectSettings", audit.Params(req), full); hid || p != audit.Params(req) {
		t.Errorf("权限都有时应该原样给：%s", p)
	}
	limited := &access{perms: auth.PresetOperator | auth.PermsOf(auth.PermProjectManage)}
	if p, hid := hideSettingsParams("UpdateProjectSettings", `{"truncated":true,"head":"{\"settings\":{\"egress\":{\"allow\":[\"10.0.0.1:22\"`, limited); !hid || p != "" {
		t.Errorf("截断过的参数应该整个不给：%q", p)
	}
}
