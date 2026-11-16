package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// L1：偷到管理员会话、做不了高权限确认的人，不能改外送目标、不能测试外送；确认过的人把已有目标改到别的地址时，
// 面板也不把保存的请求头值和密钥带过去（之前测试外送会把保存的令牌直接发到新地址）。
func TestAuditSinkNeedsElevation(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer srv.Close()
	ac, sc := h.adminWithElevation(t)
	m := baseSettings()
	m.Sinks = []*nagiv1.AuditSink{{Name: "siem", Type: "http", Url: srv.URL + "/legit", Headers: []*nagiv1.AuditSinkHeader{{Name: "Authorization", Value: "Bearer saved-token"}}}}
	res, err := sc.SetAuditSettings(ctx, connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	saved := res.Msg.GetSinks()[0]
	id := saved.GetId()

	// 同一个会话（只偷到 cookie）：退掉高权限确认。
	if _, err := h.db.ExecContext(ctx, `UPDATE sessions SET elevated_until = 0`); err != nil {
		t.Fatal(err)
	}
	_ = ac
	steal := &nagiv1.AuditSink{Id: id, Name: "siem", Type: "http", Url: srv.URL + "/attacker", Headers: []*nagiv1.AuditSinkHeader{{Name: "Authorization"}}}
	if _, err := sc.TestAuditSink(ctx, connect.NewRequest(steal)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("没有高权限确认时测试外送应被拒绝：%v", err)
	}
	// 加一个发往自己地址的目标，之后持续收到审计流。
	m.Sinks = []*nagiv1.AuditSink{saved, {Name: "evil", Type: "http", Url: srv.URL + "/attacker"}}
	if _, err := sc.SetAuditSettings(ctx, connect.NewRequest(m)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("没有高权限确认时修改外送目标应被拒绝：%v", err)
	}
	// 只改保留时间（外送目标原样）不受影响。
	m.Sinks = []*nagiv1.AuditSink{saved}
	m.RetentionDays = 120
	m.LongRetentionDays = 400
	if _, err := sc.SetAuditSettings(ctx, connect.NewRequest(m)); err != nil {
		t.Fatalf("外送目标没变时不要求确认：%v", err)
	}

	// 确认过身份：把目标改到别的地址，请求头的值留空，不沿用保存的令牌。
	if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: "admin-password-1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.TestAuditSink(ctx, connect.NewRequest(steal)); err == nil {
		t.Fatal("地址变了、请求头的值留空时应要求重新填写")
	}
	m.Sinks = []*nagiv1.AuditSink{steal}
	if _, err := sc.SetAuditSettings(ctx, connect.NewRequest(m)); err == nil {
		t.Fatal("地址变了、请求头的值留空时应要求重新填写")
	}
	mu.Lock()
	defer mu.Unlock()
	if v, ok := got["/attacker"]; ok {
		t.Fatalf("保存的令牌被发到了新地址：%q", v)
	}
	// 地址没变：留空照常沿用（发测试消息到原来的地址）。
	mu.Unlock()
	_, err = sc.TestAuditSink(ctx, connect.NewRequest(&nagiv1.AuditSink{Id: id, Name: "siem", Type: "http", Url: srv.URL + "/legit", Headers: []*nagiv1.AuditSinkHeader{{Name: "Authorization"}}}))
	mu.Lock()
	if err != nil || got["/legit"] != "Bearer saved-token" {
		t.Fatalf("地址没变时应沿用保存的请求头：%v %v", err, got)
	}
}
