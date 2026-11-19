package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
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

	// 没有「审计日志」的成员不能搜索审计记录，只能查一个项目的告警，文件的记录不在其中。
	for who, c := range map[string]nagiv1connect.SettingsServiceClient{"只读成员": vc, "操作员": oc} {
		_, err := c.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{Filter: &nagiv1.AuditFilter{Search: "payroll"}}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s搜索审计记录：%v", who, err)
		}
		if got := h.list(c, &nagiv1.AuditFilter{ProjectId: "p1", Categories: []string{audit.Alert}, Search: "payroll"}); len(got) != 0 {
			t.Errorf("%s查项目告警搜到了文件的记录：%d 条", who, len(got))
		}
	}
	if got := len(h.list(ac, &nagiv1.AuditFilter{Search: "payroll"})); got != 3 {
		t.Errorf("管理员按路径：%d 条，应该是 3 条", got)
	}

	// 过滤本身：看不到路径的动作只按容器匹配，作为上面那道限制以外的兜底。
	count := func(f audit.Filter) int {
		t.Helper()
		rows, err := h.a.view().List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	for _, c := range []struct {
		name string
		f    audit.Filter
		want int
	}{
		{"看不到任何路径，按路径", audit.Filter{Search: "payroll", SearchFilePaths: []string{}}, 0},
		{"看不到任何路径，按路径的片段", audit.Filter{Search: "payroll-2026.x", SearchFilePaths: []string{}}, 0},
		{"看不到任何路径，按冒号", audit.Filter{Search: ":", SearchFilePaths: []string{}}, 0},
		{"看不到任何路径，按容器", audit.Filter{Search: "web", SearchFilePaths: []string{}}, 2},
		{"只看得到下载的路径", audit.Filter{Search: "payroll", SearchFilePaths: []string{"DownloadVolumeFile"}}, 2},
		{"只看得到下载的路径，按删除的路径", audit.Filter{Search: "old-payroll", SearchFilePaths: []string{"DownloadVolumeFile"}}, 0},
		{"不限制", audit.Filter{Search: "payroll"}, 3},
	} {
		if got := count(c.f); got != c.want {
			t.Errorf("%s：%d 条，应该是 %d 条", c.name, got, c.want)
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

	// 没有「审计日志」的成员只在项目的事件里看到这些记录（只有失败详情），有「审计日志」的和管理员一样。
	// 这里直接用查看者检查清理规则：审计条目的参数和失败详情、项目事件的失败详情。
	rows, err := h.a.view().List(ctx, audit.Filter{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	x := memberAccess(auth.PresetOperator | auth.PermsOf(auth.PermProjectManage))
	member := auditViewer{x: x, other: "其他节点", scrub: func(s string) string { return s }, refs: i18n.PlainRefs}
	admin := auditViewer{admin: true, scrub: func(s string) string { return s }, refs: i18n.PlainRefs}
	byAction := func(v auditViewer) map[string]*nagiv1.AuditEvent {
		out := map[string]*nagiv1.AuditEvent{}
		for _, r := range rows {
			out[r.Action] = v.entry(r, auditNames{})
		}
		return out
	}
	op := byAction(member)
	for _, action := range []string{"UpdateProjectSettings", "mcp:update_project"} {
		e := op[action]
		if e == nil {
			t.Fatalf("没有 %s 的记录", action)
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
	for _, r := range rows {
		if r.Action == "UpdateProjectSettings" && strings.Contains(member.activityEntry(r, auditNames{}).GetDetail(), "192.168.1.20") {
			t.Error("项目事件里不该有出站放行的地址")
		}
	}
	adm := byAction(admin)
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
