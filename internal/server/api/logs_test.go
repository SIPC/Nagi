package api

import (
	"net/http/cookiejar"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

func TestNodeLogQueryRequiresAdminAndNeverHidesMeshFailure(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	for _, u := range []struct {
		name string
		role int64
		want connect.Code
	}{{"viewer", auth.ViewerRoleID, connect.CodePermissionDenied}, {"admin", auth.AdminRoleID, connect.CodeUnavailable}} {
		if _, err := h.a.Auth.CreateUser(t.Context(), u.name, "password-for-logs", u.role); err != nil {
			t.Fatal(err)
		}
		jar, _ := cookiejar.New(nil)
		hc := *h.srv.Client()
		hc.Jar = jar
		ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
		if _, err := ac.Login(t.Context(), connect.NewRequest(&nagiv1.LoginRequest{Username: u.name, Password: "password-for-logs"})); err != nil {
			t.Fatal(err)
		}
		lc := nagiv1connect.NewNodeLogServiceClient(&hc, h.srv.URL)
		if _, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "offline-node"})); connect.CodeOf(err) != u.want {
			t.Fatalf("%s: %v", u.name, err)
		}
	}
	lc := nagiv1connect.NewNodeLogServiceClient(h.srv.Client(), h.srv.URL)
	if _, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "n1"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("匿名查询没有被登录检查拦截: %v", err)
	}
}
