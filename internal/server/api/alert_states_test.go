package api

import (
	"net/http/cookiejar"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

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
