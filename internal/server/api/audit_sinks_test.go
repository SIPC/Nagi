package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

func (h *auditHarness) adminWithElevation(t *testing.T) (nagiv1connect.AuthServiceClient, nagiv1connect.SettingsServiceClient) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	ac, sc := h.login("admin", "admin-password-1")
	if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: "admin-password-1"})); err != nil {
		t.Fatal(err)
	}
	return ac, sc
}

func baseSettings() *nagiv1.AuditSettings {
	return &nagiv1.AuditSettings{RetentionDays: 90, LongRetentionDays: 365}
}

// L2：HTTP 接收地址常在查询串里带令牌，不能写进审计参数（审计参数还会外送到所有目标）；Access Key 返回界面时留空，
// 和文档一致，界面原样保存时沿用已保存的。
func TestAuditSinkRedaction(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	_, sc := h.adminWithElevation(t)
	m := baseSettings()
	m.Sinks = []*nagiv1.AuditSink{
		{Name: "siem", Type: "http", Url: "https://logs.example.com/ingest?token=url-secret"},
		{Name: "bucket", Type: "s3", Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "AKIDEXAMPLE", SecretKey: "sk"},
	}
	res, err := sc.SetAuditSettings(ctx, connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range res.Msg.GetSinks() {
		if sk.GetAccessKey() != "" || sk.GetSecretKey() != "" {
			t.Fatalf("返回界面时 Access Key 和 Secret Key 应留空：%v", sk)
		}
	}
	// 界面原样保存（Access Key 留空）：沿用已保存的。
	m.Sinks = res.Msg.GetSinks()
	m.RetentionDays = 120
	if _, err := sc.SetAuditSettings(ctx, connect.NewRequest(m)); err != nil {
		t.Fatal(err)
	}
	for _, sk := range h.a.Sinks(ctx) {
		if sk.Type == "s3" && (sk.AccessKey != "AKIDEXAMPLE" || sk.SecretKey != "sk") {
			t.Fatalf("留空时应沿用已保存的密钥：%+v", sk)
		}
	}
	rows, err := h.a.Audit.DB().Query(`SELECT params FROM events WHERE action = 'SetAuditSettings'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		n++
		if strings.Contains(p, "url-secret") || strings.Contains(p, "AKIDEXAMPLE") {
			t.Fatalf("审计参数里有接收地址的令牌或 Access Key：%s", p)
		}
	}
	if n == 0 {
		t.Fatal("没有 SetAuditSettings 的审计记录")
	}
}
