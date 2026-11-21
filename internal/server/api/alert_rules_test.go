package api

import (
	"log/slog"
	"net/http/cookiejar"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/store"
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
