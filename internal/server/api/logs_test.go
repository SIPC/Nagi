package api

import (
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/standby"
)

func TestNodeLogQueryRequiresAdminAndNeverHidesMeshFailure(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Nodes = nodes.NewRegistry(h.a.Q, h.a.Log, h.a.Sealer, nil)
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Logs = logs
	if err := logs.Record(t.Context(), logstore.Event{Category: logstore.Runtime, Kind: "host.journal", Data: "local marker 20%"}); err != nil {
		t.Fatal(err)
	}
	copies, err := logs.OpenCopies(t.Context(), logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer copies.Close()
	h.a.Nodes.LogCopies = copies
	if _, err := copies.Receive(t.Context(), "offline-node", logstore.CopyBatch{Source: "s", Category: logstore.Audit, Through: 5, Events: []logstore.Event{{Seq: 5, Time: time.Now(), Category: logstore.Audit, Kind: "operation", Data: "copy survives"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := copies.Receive(t.Context(), "panel:fingerprint", logstore.CopyBatch{Source: "panel-source", Category: logstore.Audit, Through: 9, Events: []logstore.Event{{Seq: 9, Time: time.Now(), Category: logstore.Audit, Kind: "panel.operation", Data: "panel marker 20%"}}}); err != nil {
		t.Fatal(err)
	}
	auditor, err := h.a.Q.CreateRole(t.Context(), "audit-only", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.Q.SetRolePermissions(t.Context(), auditor, []string{"panel.audit"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		name string
		role int64
		want connect.Code
	}{{"viewer", auth.ViewerRoleID, connect.CodePermissionDenied}, {"auditor", auditor, connect.CodePermissionDenied}, {"admin", auth.AdminRoleID, connect.CodeUnavailable}} {
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
		local, localErr := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{PanelLocal: true, Query: &nagiv1.LogQuery{Search: "MARKER 20%", KindPrefix: "host."}}))
		if u.role != auth.AdminRoleID {
			if connect.CodeOf(localErr) != connect.CodePermissionDenied {
				t.Fatalf("local permissions: %v", localErr)
			}
		} else {
			if localErr != nil || local.Msg.GetRecentCopy() || len(local.Msg.GetEntries()) != 1 {
				t.Fatalf("local search: %v %v", local, localErr)
			}
			_, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{PanelLocal: true, Query: &nagiv1.LogQuery{Source: "replaced-source"}}))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("source replacement not detected: %v", err)
			}
		}
		if _, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "offline-node"})); connect.CodeOf(err) != u.want {
			t.Fatalf("%s: %v", u.name, err)
		}
		page, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "offline-node", RecentCopy: true, Query: &nagiv1.LogQuery{Category: logstore.Audit}}))
		if u.role != auth.AdminRoleID {
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("copy permissions: %v", err)
			}
		} else if err != nil || !page.Msg.GetRecentCopy() || len(page.Msg.GetEntries()) != 1 || page.Msg.GetNextAfter() != 5 {
			t.Fatalf("explicit offline copy: %v %v", page, err)
		}
		panel, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "panel:fingerprint", RecentCopy: true, Query: &nagiv1.LogQuery{Category: logstore.Audit, Search: "MARKER 20%", KindPrefix: "panel."}}))
		if u.role != auth.AdminRoleID {
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("panel copy permissions: %v", err)
			}
		} else {
			if err != nil || panel.Msg.Source != "panel-source" || len(panel.Msg.Entries) != 1 || panel.Msg.Entries[0].Seq != 9 {
				t.Fatalf("panel identity or filters lost: %v %v", panel, err)
			}
			h.a.Panel = &PanelInfo{Standby: &standby.Client{}}
			_, err = lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "offline-node"}))
			if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "面板 mesh 没有启动") {
				t.Fatalf("standby query did not reach the mesh path: %v", err)
			}
			h.a.Panel = nil
		}
	}
	lc := nagiv1connect.NewNodeLogServiceClient(h.srv.Client(), h.srv.URL)
	if _, err := lc.QueryNodeLogs(t.Context(), connect.NewRequest(&nagiv1.NodeLogQuery{NodeId: "n1"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("匿名查询没有被登录检查拦截: %v", err)
	}
}
