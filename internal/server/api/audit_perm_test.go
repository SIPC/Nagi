package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 「审计日志」是单独的权限（见 docs/audit.md#权限）：有它才能查询、查看、导出、校验审计；没有时这些接口一律拒绝，
// 只能查本项目的告警（出站限制用）。审计设置归「安全设置」，只给管理员。预置的操作员、只读默认没有。
func TestAuditPermission(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	for _, u := range []struct {
		name string
		role int64
	}{{"admin", auth.AdminRoleID}, {"op", auth.OperatorRoleID}, {"viewer", auth.ViewerRoleID}, {"auditor", h.auditorRole(ctx)}} {
		if _, err := h.a.Auth.CreateUser(ctx, u.name, u.name+"-password-1", u.role); err != nil {
			t.Fatal(err)
		}
	}
	actx := auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin})
	login := h.a.Record(actx, audit.Event{Category: audit.Login, Action: "Login"})
	h.a.Record(actx, audit.Event{Category: audit.Operation, Action: "Deploy", ProjectID: "p1", TargetType: "container", TargetID: "web", TargetName: "web"})
	h.a.Record(actx, audit.Event{Category: audit.Alert, Action: "firing", ProjectID: "p1", TargetType: "container", TargetID: "web", TargetName: "web",
		Params: `{"title":"web","key":"egress:p1/web/198.51.100.9:443"}`})

	aac, auditor := h.login("auditor", "auditor-password-1")
	_, op := h.login("op", "op-password-1")
	_, viewer := h.login("viewer", "viewer-password-1")
	_, admin := h.login("admin", "admin-password-1")

	exportErr := func(c nagiv1connect.SettingsServiceClient) error {
		stream, err := c.ExportAuditEvents(ctx, connect.NewRequest(&nagiv1.ExportAuditEventsRequest{}))
		if err != nil {
			return err
		}
		for stream.Receive() {
		}
		return stream.Err()
	}
	calls := map[string]func(c nagiv1connect.SettingsServiceClient) error{
		"ListAuditEvents": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{}))
			return err
		},
		"ListAuditEvents 按项目": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{Filter: &nagiv1.AuditFilter{ProjectId: "p1"}}))
			return err
		},
		"ListAuditEvents 下级记录": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{ParentId: login}))
			return err
		},
		"GetAuditEvent": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.GetAuditEvent(ctx, connect.NewRequest(&nagiv1.GetAuditEventRequest{Id: login}))
			return err
		},
		"ExportAuditEvents": exportErr,
		"VerifyAudit": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.VerifyAudit(ctx, connect.NewRequest(&nagiv1.VerifyAuditRequest{}))
			return err
		},
		"ListActivity": func(c nagiv1connect.SettingsServiceClient) error {
			_, err := c.ListActivity(ctx, connect.NewRequest(&nagiv1.ListActivityRequest{}))
			return err
		},
	}
	for name, call := range calls {
		for who, c := range map[string]nagiv1connect.SettingsServiceClient{"管理员": admin, "审计员": auditor} {
			if err := call(c); err != nil {
				t.Errorf("%s 调用 %s：%v", who, name, err)
			}
		}
		for who, c := range map[string]nagiv1connect.SettingsServiceClient{"操作员": op, "只读": viewer} {
			if err := call(c); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s 调用 %s 应被拒绝：%v", who, name, err)
			}
		}
	}

	// 审计员和管理员一样看全部记录：项目以外的登录也看得到。
	if got := actions2(h.list(auditor, nil)); !slices.Contains(got, "Login") || !slices.Contains(got, "Deploy") {
		t.Errorf("审计员看到的记录：%v", got)
	}
	// 没有「审计日志」时只能查本项目的告警。
	for who, c := range map[string]nagiv1connect.SettingsServiceClient{"操作员": op, "只读": viewer} {
		if ev := h.list(c, &nagiv1.AuditFilter{ProjectId: "p1", Categories: []string{audit.Alert}}); actions(ev) != "firing" {
			t.Errorf("%s 查本项目的告警：%s", who, actions(ev))
		}
	}

	// 审计设置归「安全设置」：只能看审计的人改不了录制、保留时间和外送。
	if _, err := auditor.GetAuditSettings(ctx, connect.NewRequest(&nagiv1.GetAuditSettingsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("审计员读审计设置：%v", err)
	}
	if _, err := auditor.SetAuditSettings(ctx, connect.NewRequest(&nagiv1.AuditSettings{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("审计员改审计设置：%v", err)
	}
	if _, err := admin.GetAuditSettings(ctx, connect.NewRequest(&nagiv1.GetAuditSettingsRequest{})); err != nil {
		t.Errorf("管理员读审计设置：%v", err)
	}

	// 界面按 Me 返回的权限显示审计页。
	me, err := aac.Me(ctx, connect.NewRequest(&nagiv1.MeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(me.Msg.GetPermissions(), []string{"panel.audit"}) {
		t.Errorf("审计员的权限：%v", me.Msg.GetPermissions())
	}

	// 预置的操作员、只读默认没有「审计日志」，管理员能把它授予别的角色。
	roles, err := admin.ListRoles(ctx, connect.NewRequest(&nagiv1.ListRolesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles.Msg.GetRoles() {
		has := slices.Contains(r.GetPermissions(), "panel.audit")
		if (r.GetBuiltin() == auth.RoleAdmin) != has && r.GetBuiltin() != "" {
			t.Errorf("预置角色 %s 的审计日志：%v", r.GetBuiltin(), has)
		}
	}
	if !slices.Contains(roles.Msg.GetGrantable(), "panel.audit") || slices.Contains(roles.Msg.GetGrantable(), "panel.security") {
		t.Errorf("管理员能授予的权限：%v", roles.Msg.GetGrantable())
	}
}

func actions2(events []*nagiv1.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.GetAction())
	}
	return out
}

// 项目设置的出站限制列出最近被记录的目标：没有「审计日志」的成员（要「查看配置」才看得到告警的参数）不带搜索词取本项目的告警，在页面上按键筛
// （web/src/pages/project-egress.tsx）。非管理员的搜索不匹配参数，按「egress:」搜索一条也查不到。
func TestEgressHitsWithoutAudit(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	id, err := h.a.Q.CreateRole(ctx, "网络", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.Q.SetRolePermissions(ctx, id, []string{"project.view", "project.config", "project.network", "project.addresses"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "net", "net-password-1", id); err != nil {
		t.Fatal(err)
	}
	actx := auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin})
	h.a.Record(actx, audit.Event{Category: audit.Alert, Action: "firing", ProjectID: "p1", TargetType: "container", TargetID: "web", TargetName: "web",
		Params: `{"title":"web","key":"egress:p1/web/198.51.100.9:443"}`})
	_, net := h.login("net", "net-password-1")

	ev := h.list(net, &nagiv1.AuditFilter{ProjectId: "p1", Categories: []string{audit.Alert}})
	if len(ev) != 1 || !strings.Contains(ev[0].GetParams(), `egress:p1/web/198.51.100.9:443`) {
		t.Fatalf("本项目的告警：%v", ev)
	}
	if ev := h.list(net, &nagiv1.AuditFilter{ProjectId: "p1", Categories: []string{audit.Alert}, Search: "egress:"}); len(ev) != 0 {
		t.Errorf("非管理员的搜索不应匹配参数：%d 条", len(ev))
	}
}
