package api

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// M9：搜索只匹配查看者看得到的字段。之前只读成员能按有没有结果逐字猜出隐藏的参数（命令行里的密码），
// 操作员能确认管理员的来源 IP。
func TestAuditSearchHiddenFields(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	for _, u := range []struct{ name, role string }{{"admin", auth.RoleAdmin}, {"viewer", auth.RoleViewer}, {"op", auth.RoleOperator}} {
		if _, err := h.a.Auth.CreateUser(ctx, u.name, u.name+"-password-1", auth.BuiltinRoleID(u.role)); err != nil {
			t.Fatal(err)
		}
	}
	actx := auth.WithMeta(auth.WithUser(ctx, auth.User{ID: 1, Name: "admin", Role: auth.RoleAdmin}), auth.Meta{IP: "203.0.113.77"})
	h.a.Record(actx, audit.Event{Category: audit.Session, Action: "ExecCommand", ProjectID: "p1", TargetType: "container", TargetID: "db", TargetName: "db",
		Params: `{"command":"mysql -uroot -pS3cr3tPw"}`, Error: "compose: secret-in-error", Result: audit.ResultFailed})

	_, vc := h.login("viewer", "viewer-password-1")
	_, oc := h.login("op", "op-password-1")
	_, ac := h.login("admin", "admin-password-1")
	n := func(c nagiv1connect.SettingsServiceClient, q string) int {
		return len(h.list(c, &nagiv1.AuditFilter{Search: q}))
	}
	cases := []struct {
		who  string
		c    nagiv1connect.SettingsServiceClient
		q    string
		want int
	}{
		{"只读成员按参数", vc, "S3cr3t", 0},
		{"只读成员按失败详情", vc, "secret-in-error", 0},
		{"只读成员按 IP", vc, "203.0.113.77", 0},
		{"只读成员按对象", vc, "db", 1},
		{"只读成员按动作", vc, "ExecCommand", 1},
		// 参数和失败详情显示前要去掉别人的 IP 和看不到的节点，搜索不能匹配原文（2026-11-19-87c02b5#10）。
		{"操作员按参数", oc, "S3cr3t", 0},
		{"操作员按失败详情", oc, "secret-in-error", 0},
		{"操作员按对象", oc, "db", 1},
		{"操作员按 IP", oc, "203.0.113.77", 0},
		{"管理员按 IP", ac, "203.0.113.77", 1},
		{"管理员按参数", ac, "S3cr3t", 1},
	}
	for _, c := range cases {
		if got := n(c.c, c.q); got != c.want {
			t.Errorf("%s「%s」：%d 条，应该是 %d 条", c.who, c.q, got, c.want)
		}
	}
}
