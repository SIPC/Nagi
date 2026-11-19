package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// auditHarness 是挂好全部接口的测试面板：真实的登录、拦截器和审计库。
type auditHarness struct {
	t        *testing.T
	a        *API
	srv      *httptest.Server
	settings nagiv1connect.SettingsServiceClient
	authc    nagiv1connect.AuthServiceClient
	db       *sql.DB
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	key, err := sealer.Load(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	q := store.New(d)
	as := auth.New(q, key)
	as.SetAudit(log)
	a := &API{Q: q, Audit: log, Auth: as, Sealer: key, Log: slog.Default(), Panel: &PanelInfo{Q: q, DataDir: dir}}
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return &auditHarness{t: t, a: a, srv: srv, db: d}
}

// client 返回带着 cookie 的客户端，模拟一个浏览器。
func (h *auditHarness) client() (nagiv1connect.AuthServiceClient, nagiv1connect.SettingsServiceClient) {
	jar, _ := cookiejar.New(nil)
	hc := h.srv.Client()
	c := *hc
	c.Jar = jar
	return nagiv1connect.NewAuthServiceClient(&c, h.srv.URL), nagiv1connect.NewSettingsServiceClient(&c, h.srv.URL)
}

func (h *auditHarness) login(username, password string) (nagiv1connect.AuthServiceClient, nagiv1connect.SettingsServiceClient) {
	h.t.Helper()
	ac, sc := h.client()
	req := connect.NewRequest(&nagiv1.LoginRequest{Username: username, Password: password})
	if _, err := ac.Login(context.Background(), req); err != nil {
		h.t.Fatal(err)
	}
	return ac, sc
}

// loginElevated 登录并做高权限确认（新建、修改成员要先确认）。
func (h *auditHarness) loginElevated(username, password string) (nagiv1connect.AuthServiceClient, nagiv1connect.SettingsServiceClient) {
	h.t.Helper()
	ac, sc := h.login(username, password)
	if _, err := ac.Elevate(context.Background(), connect.NewRequest(&nagiv1.ElevateRequest{Password: password})); err != nil {
		h.t.Fatal(err)
	}
	return ac, sc
}

func (h *auditHarness) list(sc nagiv1connect.SettingsServiceClient, f *nagiv1.AuditFilter) []*nagiv1.AuditEvent {
	h.t.Helper()
	res, err := sc.ListAuditEvents(context.Background(), connect.NewRequest(&nagiv1.ListAuditEventsRequest{Filter: f, Limit: 200}))
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Msg.GetEvents()
}

func actions(events []*nagiv1.AuditEvent) string {
	var out []string
	for _, e := range events {
		out = append(out, e.GetAction())
	}
	return strings.Join(out, ",")
}

// 第 1 步的完成标准（见 docs/security.md#三实施顺序）：密码、令牌、变量值不出现在 params 里；界面能按每个条件筛选。
func TestAuditRecordsAndFilters(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	// 登录失败：同一 IP 第一次单独记一条，之后只计数。
	ac, _ := h.client()
	for range 3 {
		_, _ = ac.Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "admin", Password: "wrong-password"}))
	}
	_, sc := h.loginElevated("admin", "admin-password-1")

	// 新增成员：请求里的密码不能出现在参数里。
	const secret = "Very-Secret-Password-42"
	if _, err := sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "bob", Password: secret, Role: auth.RoleOperator})); err != nil {
		t.Fatal(err)
	}
	// 失败的操作也记：重复的用户名。
	_, _ = sc.CreateUser(ctx, connect.NewRequest(&nagiv1.CreateUserRequest{Username: "bob", Password: secret, Role: auth.RoleOperator}))
	// 重置成员的密码（id 为 2 的是 bob），要先用密码做高权限确认。
	if _, err := h.db.ExecContext(ctx, `UPDATE sessions SET elevated_until = ?, elevated_by = 'password'`, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.UpdateUser(ctx, connect.NewRequest(&nagiv1.UpdateUserRequest{Id: 2, Role: auth.RoleOperator, Password: secret})); err != nil {
		t.Fatal(err)
	}

	all := h.list(sc, nil)
	if len(all) == 0 {
		t.Fatal("没有记录")
	}
	for _, e := range all {
		if strings.Contains(e.GetParams(), secret) || strings.Contains(e.GetError(), secret) {
			t.Fatalf("%s 的记录里出现了密码：%s", e.GetAction(), e.GetParams())
		}
	}
	got := actions(all)
	for _, want := range []string{"login_failed", "Login", "CreateUser", "UpdateUser"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s：%s", want, got)
		}
	}
	if strings.Count(got, "login_failed") != 1 {
		t.Errorf("同一 IP 的登录失败应只单独记一条：%s", got)
	}

	// 每个筛选条件。
	if ev := h.list(sc, &nagiv1.AuditFilter{Categories: []string{audit.Login}}); actions(ev) == "" || strings.Contains(actions(ev), "CreateUser") {
		t.Errorf("按类别：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{FailedOnly: true}); !strings.Contains(actions(ev), "CreateUser") || strings.Contains(actions(ev), "Login,") {
		t.Errorf("只看失败：%s", actions(ev))
	}
	admin := ""
	for _, e := range all {
		if e.GetAction() == "CreateUser" {
			admin = e.GetActorId()
			if e.GetTargetName() != "bob" || e.GetIp() == "" || e.GetSession() == "" || e.GetVia() != audit.ViaWeb {
				t.Errorf("字段不全：%+v", e)
			}
		}
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{ActorId: admin}); strings.Contains(actions(ev), "login_failed") {
		t.Errorf("按成员：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{Via: audit.ViaMCP}); len(ev) != 0 {
		t.Errorf("按来源：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{Search: "bob"}); !strings.Contains(actions(ev), "CreateUser") || strings.Contains(actions(ev), "Login") {
		t.Errorf("搜索：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{From: timestamppb.New(time.Now().Add(time.Hour))}); len(ev) != 0 {
		t.Errorf("时间范围：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{ProjectId: "nope"}); len(ev) != 0 {
		t.Errorf("按项目：%s", actions(ev))
	}
	if ev := h.list(sc, &nagiv1.AuditFilter{NodeId: "nope"}); len(ev) != 0 {
		t.Errorf("按节点：%s", actions(ev))
	}

	// 操作员没有「审计日志」：查不了审计。
	_, bob := h.login("bob", secret)
	if _, err := bob.ListAuditEvents(ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("操作员查询审计：%v", err)
	}
	if _, err := bob.ExportAuditEvents(ctx, connect.NewRequest(&nagiv1.ExportAuditEventsRequest{})); err == nil {
		// 服务端流的错误在第一次 Receive 时才返回。
		stream, _ := bob.ExportAuditEvents(ctx, connect.NewRequest(&nagiv1.ExportAuditEventsRequest{}))
		for stream.Receive() {
		}
		if stream.Err() == nil {
			t.Error("操作员不能导出")
		}
	}

	// 管理员导出 JSON Lines，带哈希。
	stream, err := sc.ExportAuditEvents(ctx, connect.NewRequest(&nagiv1.ExportAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for stream.Receive() {
		out.Write(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"hash"`) || strings.Contains(out.String(), secret) {
		t.Errorf("导出：%s", out.String())
	}
}

// 旧的活动记录全部迁移进审计库，迁移只做一次。
func TestMigrateActivity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q := store.New(d)
	for i, p := range []store.InsertActivityParams{
		{CreatedAt: 100, Kind: "operation", Actor: "admin", Action: "Deploy", ProjectID: "p1", Target: "web, db"},
		{CreatedAt: 101, Kind: "operation", Actor: "admin", Action: "NodeExec", Target: "node1", Detail: "ls: timeout", Failed: 1, Via: "mcp:claude"},
		{CreatedAt: 102, Kind: "alert", Actor: "node1", Action: "firing", Detail: "CPU 过高"},
		{CreatedAt: 103, Kind: "operation", Actor: "x", Action: "Login", Failed: 1, Detail: "用户名或密码不正确"},
	} {
		if err := q.InsertActivity(ctx, p); err != nil {
			t.Fatal(i, err)
		}
	}
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	n, err := log.MigrateActivity(ctx, d)
	if err != nil || n != 4 {
		t.Fatalf("迁移 %d 条, %v", n, err)
	}
	if n, _ := log.MigrateActivity(ctx, d); n != 0 {
		t.Fatalf("重复迁移了 %d 条", n)
	}
	rows, _ := log.List(ctx, audit.Filter{Limit: 10})
	if len(rows) != 4 {
		t.Fatalf("%d 条", len(rows))
	}
	byAction := map[string]audit.Event{}
	for _, r := range rows {
		byAction[r.Action] = r
	}
	if e := byAction["NodeExec"]; e.Category != audit.Session || e.Via != audit.ViaMCP || e.Client != "claude" || e.Result != audit.ResultFailed || e.TargetType != "node" {
		t.Errorf("NodeExec: %+v", e)
	}
	if e := byAction["firing"]; e.Category != audit.Alert || !strings.Contains(e.Params, "CPU") {
		t.Errorf("告警: %+v", e)
	}
	if e := byAction["Login"]; e.Category != audit.Login || e.Time.Unix() != 103 {
		t.Errorf("登录: %+v", e)
	}
	// 原来的接口照常返回旧的格式。
	a := &API{Q: q, Audit: log}
	res, err := (&projectService{a}).ListProjectEvents(auth.WithUser(ctx, auth.User{ID: 1, Role: auth.RoleAdmin}),
		connect.NewRequest(&nagiv1.ListProjectEventsRequest{ProjectId: "p1", Container: "db"}))
	if err != nil || len(res.Msg.GetEntries()) != 1 || res.Msg.GetEntries()[0].GetAction() != "Deploy" {
		t.Fatalf("ListProjectEvents: %v %v", res, err)
	}
}
