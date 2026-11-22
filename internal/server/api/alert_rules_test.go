package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 只能管项目 p1 告警的成员：只看到 p1 的覆盖（看不到 p2 和节点的），改不了全局默认值，
// 伪造 p2 或节点的覆盖当作不存在。
func TestAlertRulesAPIScope(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.New(slog.DiscardHandler), Nodes: h.a.Nodes}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO roles(id,name) VALUES(10,'alert-rules')`)
	exec(`INSERT INTO role_permissions(role_id,permission) VALUES(10,'project.view'),(10,'project.alerts')`)
	id, err := h.a.Auth.CreateUser(ctx, "rules", "rules-test-pass", 10)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE users SET all_projects=0,all_nodes=0 WHERE id=?`, id)
	exec(`INSERT INTO projects(id,name,network_name,revision,created_at,settings) VALUES('p1','one','one',1,0,x''),('p2','two','two',1,0,x'')`)
	if err := h.a.Q.CreateNode(ctx, store.CreateNodeParams{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO user_projects(user_id,project_id) VALUES(?,'p1')`, id)
	restart := map[string]*nagiv1.AlertRule{alert.RuleRestart: {Threshold: proto.Float64(9)}}
	for _, o := range []*nagiv1.AlertOverride{
		{Scope: "project", ProjectId: "p1", Rules: restart},
		{Scope: "project", ProjectId: "p2", Rules: restart},
		{Scope: "node", NodeId: "n1", Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {Threshold: proto.Float64(95)}}},
	} {
		if _, err := h.a.Nodes.SetAlertOverride(ctx, o, 0); err != nil {
			t.Fatal(err)
		}
	}

	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "rules", Password: "rules-test-pass"})); err != nil {
		t.Fatal(err)
	}
	client := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	got, err := client.GetAlertRules(ctx, connect.NewRequest(&nagiv1.GetAlertRulesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if o := got.Msg.GetOverrides(); len(o) != 1 || o[0].GetProjectId() != "p1" || got.Msg.GetCanEditPolicy() {
		t.Fatalf("应只看到 p1 的覆盖、不能改全局：%v", got.Msg)
	}
	if _, err := client.UpdateAlertPolicy(ctx, connect.NewRequest(&nagiv1.UpdateAlertPolicyRequest{Policy: &nagiv1.AlertPolicy{}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("改全局默认值应被拒绝：%v", err)
	}
	set := func(o *nagiv1.SetAlertOverrideRequest) error {
		_, err := client.SetAlertOverride(ctx, connect.NewRequest(o))
		return err
	}
	if err := set(&nagiv1.SetAlertOverrideRequest{Scope: "project", ProjectId: "p2", Rules: restart, BaseRevision: 1}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("别的项目当作不存在：%v", err)
	}
	if err := set(&nagiv1.SetAlertOverrideRequest{Scope: "container", ProjectId: "p2", Container: "x", Rules: restart}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("别的项目的容器当作不存在：%v", err)
	}
	if err := set(&nagiv1.SetAlertOverrideRequest{Scope: "node", NodeId: "n1", BaseRevision: 1}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("范围外的节点当作不存在：%v", err)
	}
	if err := set(&nagiv1.SetAlertOverrideRequest{Scope: "project", ProjectId: "p1", Rules: map[string]*nagiv1.AlertRule{alert.RuleRestart: {Threshold: proto.Float64(4)}}, BaseRevision: 1}); err != nil {
		t.Fatalf("自己的项目可以改：%v", err)
	}
	all, _ := h.a.Nodes.AlertOverrides(ctx)
	for _, o := range all {
		if o.GetProjectId() == "p2" && o.GetRules()[alert.RuleRestart].GetThreshold() != 9 || o.GetScope() == "node" && o.GetRevision() != 1 {
			t.Fatalf("范围外的覆盖被改了：%v", o)
		}
	}
}

// 事件通知开关：安全类只有管理员能改；关掉要高权限确认，并且在保存之前先发到当前渠道；打开不用确认。
func TestSetAlertNotificationPermissionsAndNoticeFirst(t *testing.T) {
	h := newAuditHarness(t)
	alert.AllowLoopbackForTest(t)
	ctx := t.Context()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Projects = &projects.Service{Q: h.a.Q, Sealer: h.a.Sealer, Log: slog.New(slog.DiscardHandler), Nodes: h.a.Nodes}
	var mu sync.Mutex
	type got struct {
		title  string
		offNow bool
	}
	var received []got
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		p, _ := h.a.Nodes.AlertPolicy(ctx)
		_, off := p.GetNotifications()["token"]
		mu.Lock()
		received = append(received, got{body.Title, off})
		mu.Unlock()
	}))
	defer hook.Close()
	if err := h.a.Nodes.SetAlertConfig(ctx, &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c1", Name: "ops", Type: "webhook", Url: hook.URL}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO roles(id,name) VALUES(11,'settings')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO role_permissions(role_id,permission) VALUES(11,'panel.settings')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Auth.CreateUser(ctx, "ops", "ops-test-pass-1", 11); err != nil {
		t.Fatal(err)
	}
	clientFor := func(user, pass string) (nagiv1connect.AuthServiceClient, nagiv1connect.AlertServiceClient) {
		jar, _ := cookiejar.New(nil)
		hc := *h.srv.Client()
		hc.Jar = jar
		ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
		if _, err := ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: user, Password: pass})); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Elevate(ctx, connect.NewRequest(&nagiv1.ElevateRequest{Password: pass})); err != nil {
			t.Fatal(err)
		}
		return ac, nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	}
	rev := func() int64 { p, _ := h.a.Nodes.AlertPolicy(ctx); return p.GetRevision() }
	_, ops := clientFor("ops", "ops-test-pass-1")
	if _, err := ops.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "token", BaseRevision: rev()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("安全类的通知只有管理员能关：%v", err)
	}
	if _, err := ops.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "takeover", BaseRevision: rev()})); err != nil {
		t.Fatalf("有面板设置权限的成员能关顶替通知：%v", err)
	}

	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "admin", Password: "admin-password-1"})); err != nil {
		t.Fatal(err)
	}
	unelevated := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL)
	if _, err := unelevated.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "token", BaseRevision: rev()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("关掉通知要高权限确认：%v", err)
	}
	if _, err := unelevated.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "takeover", Enabled: true, BaseRevision: rev()})); err != nil {
		t.Fatalf("打开通知不用确认：%v", err)
	}
	_, admin := clientFor("admin", "admin-password-1")
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failed.Close()
	goodChannel := &nagiv1.AlertChannel{Id: "c1", Name: "ops", Type: "webhook", Url: hook.URL}
	badChannel := &nagiv1.AlertChannel{Id: "c2", Name: "unavailable", Type: "webhook", Url: failed.URL}
	// Every configured channel must receive the notice. Partial delivery must
	// not silently disable the notice for the channel that failed.
	for _, channels := range [][]*nagiv1.AlertChannel{{badChannel}, {goodChannel, badChannel}} {
		if err := h.a.Nodes.SetAlertConfig(ctx, &nagiv1.AlertConfig{Channels: channels}); err != nil {
			t.Fatal(err)
		}
		before := rev()
		if _, err := admin.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "token", BaseRevision: before})); err == nil {
			t.Fatal("failed delivery must reject disabling the notification")
		}
		if rev() != before || !h.a.Nodes.NoticeOn(ctx, "security:token:1") {
			t.Fatal("failed delivery changed the saved notification policy")
		}
	}
	if err := h.a.Nodes.SetAlertConfig(ctx, &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{goodChannel}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	received = nil
	mu.Unlock()
	if _, err := admin.SetAlertNotification(ctx, connect.NewRequest(&nagiv1.SetAlertNotificationRequest{Key: "token", BaseRevision: rev()})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].offNow || !strings.Contains(received[0].title, "生成访问令牌") {
		t.Fatalf("关掉之前应先发到当前渠道：%+v", received)
	}
	if h.a.Nodes.NoticeOn(ctx, "security:token:1") {
		t.Fatal("发完以后照常生效")
	}
}

// 基线进度：项目成员只看到自己项目的映射，看不到别的项目和节点级的，也看不到「请升级节点」的提示。
func TestListAlertBaselinesScope(t *testing.T) {
	h := newAuditHarness(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	h.a.Nodes.AlertLogs = logs
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO roles(id,name) VALUES(10,'alerts')`)
	exec(`INSERT INTO role_permissions(role_id,permission) VALUES(10,'project.view'),(10,'project.alerts')`)
	id, err := h.a.Auth.CreateUser(ctx, "member", "member-test-pass", 10)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE users SET all_projects=0,all_nodes=0 WHERE id=?`, id)
	exec(`INSERT INTO projects(id,name,network_name,revision,created_at,settings) VALUES('p1','one','one',1,0,x''),('p2','two','two',1,0,x'')`)
	exec(`INSERT INTO user_projects(user_id,project_id) VALUES(?,'p1')`, id)
	for _, n := range []string{"n1", "n2"} {
		if err := h.a.Q.CreateNode(ctx, store.CreateNodeParams{ID: n, Name: "node-" + n}); err != nil {
			t.Fatal(err)
		}
	}
	// 节点上报的账本（和 nodes.syncAlerts 写的格式一样：baselines 是 protojson）。
	var raws []json.RawMessage
	for _, b := range []*nagiv1.AlertBaseline{
		{Rule: alert.RuleL4Surge, ProjectId: "p1", Subject: "tcp", RequiredSeconds: 21600, CollectedSeconds: 9000, Status: "building"},
		{Rule: alert.RuleL4Surge, ProjectId: "p2", Subject: "secret-route", RequiredSeconds: 21600, Status: "building"},
		{Rule: alert.RuleTraffic, Subject: "node-level", Status: "ready"},
	} {
		raw, _ := protojson.Marshal(b)
		raws = append(raws, raw)
	}
	if err := logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.Alert, Kind: "alert.snapshot", Object: "n1"}, "alerts/state/n1",
		map[string]any{"baselines": raws, "baseline_at": time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "member", Password: "member-test-pass"})); err != nil {
		t.Fatal(err)
	}
	got, err := nagiv1connect.NewAlertServiceClient(&hc, h.srv.URL).ListAlertBaselines(ctx, connect.NewRequest(&nagiv1.ListAlertBaselinesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	b := got.Msg.GetBaselines()
	if len(b) != 1 || b[0].GetBaseline().GetSubject() != "tcp" || b[0].GetProject() != "one" || b[0].GetNodeId() != "" || b[0].GetNode() != "" {
		t.Fatalf("只应看到 p1 的映射，节点不可见：%v", b)
	}
}
