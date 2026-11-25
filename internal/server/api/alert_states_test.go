package api

import (
	"context"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"google.golang.org/grpc"
)

func TestStandbyAlertAPIReturnsRecentCopyWhenPrimaryTimesOut(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Nodes.AlertLogs = logs
	h.a.Nodes.PanelAlert(ctx, &nagiv1.Alert{Key: "saved-event", Title: "Saved before primary failure"})
	h.a.Nodes.ReadOnly = true
	h.a.Nodes.AlertPrimary = func(ctx context.Context) (*grpc.ClientConn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "standby-test-password", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
	if _, err := ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "admin", Password: "standby-test-password"})); err != nil {
		t.Fatal(err)
	}
	client := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	res, err := client.ListAlertStates(ctx, connect.NewRequest(&nagiv1.ListAlertStatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.RecentCopy || len(res.Msg.Alerts) != 1 || res.Msg.Alerts[0].Alert.Key != "saved-event" {
		t.Fatalf("saved alert unavailable after primary timeout: %v", res.Msg)
	}
}

func TestAlertAPIEventTitlesFollowRequestLanguage(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Nodes.AlertLogs = logs
	for _, event := range []*nagiv1.Alert{
		{Key: "session:browser:session_key_missing", Title: "登录安全", Message: "browser details"},
		{Key: "security:token:123", Title: "账号 alice 生成了访问令牌 deploy", Message: "actor details"},
		{Key: "unknown:legacy", Title: "Original legacy title", Message: "original details"},
		{Key: "event:snapshot:p1/work", Title: "容器 work 定时快照失败", Message: "repo locked"},
	} {
		h.a.Nodes.PanelAlert(ctx, event)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "language-test-password", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
	if _, err := ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "admin", Password: "language-test-password"})); err != nil {
		t.Fatal(err)
	}
	client := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	for _, tc := range []struct{ lang, session, token, snapshot string }{
		{"en", "Session missing its signing key", "Access token created", "Scheduled snapshot failed"},
		{"zh-CN", "会话拿不出签名密钥", "生成访问令牌", "定时快照失败"},
		{"en", "Session missing its signing key", "Access token created", "Scheduled snapshot failed"},
	} {
		req := connect.NewRequest(&nagiv1.ListAlertStatesRequest{})
		req.Header().Set("Accept-Language", tc.lang)
		res, err := client.ListAlertStates(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]*nagiv1.Alert)
		for _, state := range res.Msg.Alerts {
			got[state.Alert.Key] = state.Alert
		}
		for key, title := range map[string]string{"session:browser:session_key_missing": tc.session, "security:token:123": tc.token, "unknown:legacy": "Original legacy title", "event:snapshot:p1/work": tc.snapshot} {
			if got[key] == nil || got[key].Title != title {
				t.Fatalf("%s title for %s: got %v, want %q", tc.lang, key, got[key], title)
			}
		}
		if got["event:snapshot:p1/work"].Message != "容器 work 定时快照失败\nrepo locked" {
			t.Fatalf("snapshot event lost its container or detail: %v", got["event:snapshot:p1/work"])
		}
		if got["session:browser:session_key_missing"].Message != "登录安全\nbrowser details" || got["unknown:legacy"].Message != "original details" || !strings.Contains(got["security:token:123"].Message, "账号 alice 生成了访问令牌 deploy\nactor details") {
			t.Fatalf("event details lost or repeatedly prefixed: %v", res.Msg)
		}
	}
	stored, err := h.a.Nodes.AlertStates(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range stored {
		if state.Alert.Key == "session:browser:session_key_missing" && (state.Alert.Title != "登录安全" || state.Alert.Message != "browser details") {
			t.Fatalf("request language changed stored event: %v", state)
		}
	}
}

func TestAlertAPIEnforcesProjectScopePermissionsAndElevation(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Nodes.AlertLogs = logs
	h.a.Nodes.Audit = h.a.Audit
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO roles(id,name) VALUES(10,'alert-handler')`)
	exec(`INSERT INTO role_permissions(role_id,permission) VALUES(10,'project.view'),(10,'project.alerts'),(10,'panel.audit')`)
	id, err := h.a.Auth.CreateUser(ctx, "handler", "handler-test-pass", 10)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE users SET all_projects=0,all_nodes=0 WHERE id=?`, id)
	exec(`INSERT INTO projects(id,name,network_name,revision,created_at,settings) VALUES('p1','one','one',1,0,x''),('p2','two','two',1,0,x'')`)
	exec(`INSERT INTO user_projects(user_id,project_id) VALUES(?,'p1')`, id)
	for _, p := range []string{"p1", "p2", ""} {
		h.a.Nodes.PanelAlert(ctx, &nagiv1.Alert{Key: "event:" + p, Project: p, Title: "issue " + p})
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
	client := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	if _, err := ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "handler", Password: "handler-test-pass"})); err != nil {
		t.Fatal(err)
	}
	result, err := client.ListAlertStates(ctx, connect.NewRequest(&nagiv1.ListAlertStatesRequest{IncludeHistory: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Msg.Alerts) != 1 || result.Msg.Alerts[0].Alert.Project != "p1" {
		t.Fatalf("cross-project or global leak: %v", result.Msg)
	}
	a := result.Msg.Alerts[0]
	req := &nagiv1.HandleAlertRequest{Key: a.Alert.Key, Occurrence: a.Alert.Occurrence, Action: "acknowledge"}
	if _, err := client.HandleAlert(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("missing elevation accepted: %v", err)
	}
	if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: "handler-test-pass"})); err != nil {
		t.Fatal(err)
	}
	handled, err := client.HandleAlert(ctx, connect.NewRequest(req))
	if err != nil || !handled.Msg.Alert.Acknowledged {
		t.Fatalf("scoped elevated handle: %v %v", handled, err)
	}
	all, err := h.a.Nodes.AlertStates(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range all {
		if hidden.Alert.Project == "p1" {
			continue
		}
		_, err := client.HandleAlert(ctx, connect.NewRequest(&nagiv1.HandleAlertRequest{Key: hidden.Alert.Key, Occurrence: hidden.Alert.Occurrence, Action: "acknowledge"}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("hidden alert exposed: %v", err)
		}
	}
	exec(`DELETE FROM role_permissions WHERE role_id=10 AND permission='project.alerts'`)
	req.HandlingRevision = 1
	if _, err := client.HandleAlert(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("revoked handling permission accepted: %v", err)
	}
	exec(`DELETE FROM role_permissions WHERE role_id=10 AND permission='panel.audit'`)
	if _, err := client.ListAlertStates(ctx, connect.NewRequest(&nagiv1.ListAlertStatesRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("revoked audit permission accepted: %v", err)
	}
	// A project-limited user cannot see a node-global alert even on a shared node.
	svc := alertService{h.a}
	user := auth.MemberWith(id, auth.PermsOf(auth.PermView, auth.PermAudit, auth.PermAlerts, auth.PermNodeView))
	user.LimitProjects = true
	if ok, err := svc.visibleAlert(auth.WithUser(ctx, user), &nagiv1.AlertState{NodeId: "shared", Alert: &nagiv1.Alert{Key: "disk"}}); err != nil || ok {
		t.Fatalf("shared node bypassed project scope: %v %v", ok, err)
	}
}

// 一键清理告警只给管理员，要高权限确认，成功和被拒绝都记入审计，记录里有清理的条数。
func TestClearAlertsAdminOnlyElevatedAndAudited(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Nodes.AlertLogs = logs
	for _, k := range []string{"event:a", "event:b"} {
		h.a.Nodes.PanelAlert(ctx, &nagiv1.Alert{Key: k, Title: k})
	}
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO roles(id,name) VALUES(10,'alert-handler')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO role_permissions(role_id,permission) VALUES(10,'project.view'),(10,'project.alerts'),(10,'panel.audit')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "handler", "handler-test-pass", 10); err != nil {
		t.Fatal(err)
	}
	clearer := func(user, password string, elevate bool) nagiv1connect.AlertServiceClient {
		jar, _ := cookiejar.New(nil)
		hc := *h.srv.Client()
		hc.Jar = jar
		ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
		if _, err := ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: user, Password: password})); err != nil {
			t.Fatal(err)
		}
		if elevate {
			if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: password})); err != nil {
				t.Fatal(err)
			}
		}
		return nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	}
	if _, err := clearer("handler", "handler-test-pass", true).ClearAlerts(ctx, connect.NewRequest(&nagiv1.ClearAlertsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-admin with alert permissions cleared alerts: %v", err)
	}
	if _, err := clearer("admin", "admin-password-1", false).ClearAlerts(ctx, connect.NewRequest(&nagiv1.ClearAlertsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("missing elevation accepted: %v", err)
	}
	if list, _ := h.a.Nodes.AlertStates(ctx, "", false); len(list) != 2 {
		t.Fatalf("refused clears changed alerts: %v", list)
	}
	res, err := clearer("admin", "admin-password-1", true).ClearAlerts(ctx, connect.NewRequest(&nagiv1.ClearAlertsRequest{}))
	if err != nil || res.Msg.Cleared != 2 {
		t.Fatalf("elevated admin clear: %v %v", res, err)
	}
	if list, _ := h.a.Nodes.AlertStates(ctx, "", false); len(list) != 0 {
		t.Fatalf("alerts still current: %v", list)
	}
	_, sc := h.login("admin", "admin-password-1")
	var ok, denied int
	for _, e := range h.list(sc, nil) {
		if e.GetAction() != "ClearAlerts" {
			continue
		}
		switch e.GetResult() {
		case audit.ResultOK:
			ok++
			if !strings.Contains(e.GetParams(), `"cleared":2`) {
				t.Fatalf("audit record lacks the cleared count: %s", e.GetParams())
			}
		case audit.ResultDenied:
			denied++
		}
	}
	if ok != 1 || denied != 2 {
		t.Fatalf("audit: %d ok, %d denied", ok, denied)
	}
}
