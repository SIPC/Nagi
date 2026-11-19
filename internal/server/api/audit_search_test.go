package api

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// auditorRole 建一个只有「审计日志」的自定义角色，返回角色 ID。
func (h *auditHarness) auditorRole(ctx context.Context) int64 {
	h.t.Helper()
	id, err := h.a.Q.CreateRole(ctx, "审计员", time.Now().Unix())
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.a.Q.SetRolePermissions(ctx, id, []string{"panel.audit"}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// M9：搜索只匹配查看者看得到的字段。之前只读成员能按有没有结果逐字猜出隐藏的参数（命令行里的密码），
// 操作员能确认管理员的来源 IP。现在没有「审计日志」的成员只能查本项目的告警，搜索同样只匹配看得到的字段；
// 有「审计日志」的成员和管理员一样。
func TestAuditSearchHiddenFields(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	for _, u := range []struct {
		name string
		role int64
	}{{"admin", auth.AdminRoleID}, {"viewer", auth.ViewerRoleID}, {"op", auth.OperatorRoleID}, {"auditor", h.auditorRole(ctx)}} {
		if _, err := h.a.Auth.CreateUser(ctx, u.name, u.name+"-password-1", u.role); err != nil {
			t.Fatal(err)
		}
	}
	actx := auth.WithMeta(auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin}), auth.Meta{IP: "203.0.113.77"})
	h.a.Record(actx, audit.Event{Category: audit.Session, Action: "ExecCommand", ProjectID: "p1", TargetType: "container", TargetID: "db", TargetName: "db",
		Params: `{"command":"mysql -uroot -pS3cr3tPw"}`, Error: "compose: secret-in-error", Result: audit.ResultFailed})
	h.a.Record(actx, audit.Event{Category: audit.Alert, Action: "firing", ProjectID: "p1", TargetType: "container", TargetID: "db", TargetName: "db",
		Params: `{"title":"S3cr3tAlert","key":"egress:p1/db/198.51.100.9:443"}`, Error: "alert-secret-error"})

	_, vc := h.login("viewer", "viewer-password-1")
	_, oc := h.login("op", "op-password-1")
	_, ac := h.login("admin", "admin-password-1")
	_, auc := h.login("auditor", "auditor-password-1")
	n := func(c nagiv1connect.SettingsServiceClient, f *nagiv1.AuditFilter) int {
		return len(h.list(c, f))
	}
	alerts := func(q string) *nagiv1.AuditFilter {
		return &nagiv1.AuditFilter{ProjectId: "p1", Categories: []string{audit.Alert}, Search: q}
	}
	cases := []struct {
		who  string
		c    nagiv1connect.SettingsServiceClient
		f    *nagiv1.AuditFilter
		want int
	}{
		{"只读成员按告警参数", vc, alerts("S3cr3tAlert"), 0},
		{"只读成员按告警失败详情", vc, alerts("alert-secret-error"), 0},
		{"只读成员按告警对象", vc, alerts("db"), 1},
		{"操作员按告警参数", oc, alerts("198.51.100.9"), 0},
		{"操作员按告警对象", oc, alerts("db"), 1},
		{"审计员按 IP", auc, &nagiv1.AuditFilter{Search: "203.0.113.77"}, 2},
		{"审计员按参数", auc, &nagiv1.AuditFilter{Search: "S3cr3t"}, 2},
		{"管理员按 IP", ac, &nagiv1.AuditFilter{Search: "203.0.113.77"}, 2},
		{"管理员按参数", ac, &nagiv1.AuditFilter{Search: "S3cr3t"}, 2},
	}
	for _, c := range cases {
		if got := n(c.c, c.f); got != c.want {
			t.Errorf("%s「%s」：%d 条，应该是 %d 条", c.who, c.f.GetSearch(), got, c.want)
		}
	}
	// 没有「审计日志」时，告警以外的搜索一律拒绝，不按有没有结果透露任何内容。
	for _, c := range []nagiv1connect.SettingsServiceClient{vc, oc} {
		_, err := c.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{Filter: &nagiv1.AuditFilter{Search: "S3cr3t"}}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("没有审计日志权限的搜索：%v", err)
		}
	}
}
