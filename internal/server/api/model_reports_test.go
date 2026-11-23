package api

import (
	"encoding/json"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

func TestReportConfigurationAndPermissions(t *testing.T) {
	h := newAuditHarness(t)
	for _, role := range []int64{auth.AdminRoleID, auth.ViewerRoleID} {
		name := "viewer"
		if role == auth.AdminRoleID {
			name = "admin"
		}
		if _, err := h.a.Auth.CreateUser(t.Context(), name, "report-test-password", role); err != nil {
			t.Fatal(err)
		}
		jar, _ := cookiejar.New(nil)
		hc := *h.srv.Client()
		hc.Jar = jar
		ac := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL)
		if _, err := ac.Login(t.Context(), connect.NewRequest(&nagiv1.LoginRequest{Username: name, Password: "report-test-password"})); err != nil {
			t.Fatal(err)
		}
		c := nagiv1connect.NewModelReportServiceClient(&hc, h.srv.URL)
		cfg, err := c.GetReportConfig(t.Context(), connect.NewRequest(&nagiv1.GetReportConfigRequest{}))
		if role != auth.AdminRoleID {
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("viewer config: %v", err)
			}
			if _, err := c.ListReports(t.Context(), connect.NewRequest(&nagiv1.ListReportsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("viewer reports: %v", err)
			}
			continue
		}
		if err != nil || cfg.Msg.Enabled || cfg.Msg.HasApiKey {
			t.Fatalf("default: %v %v", cfg, err)
		}
		if _, err := c.GenerateReport(t.Context(), connect.NewRequest(&nagiv1.GenerateReportRequest{})); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("disabled: %v", err)
		}
		update := &nagiv1.UpdateReportConfigRequest{Enabled: true, Endpoint: "https://model.example/v1/chat/completions", Model: "fixture", ApiKey: "not-a-real-key-unique-7481"}
		if _, err := c.UpdateReportConfig(t.Context(), connect.NewRequest(update)); err != nil {
			t.Fatal(err)
		}
		stored, err := h.a.Q.GetSetting(t.Context(), reportConfigKey)
		if err != nil || strings.Contains(stored, update.ApiKey) || strings.Contains(stored, update.Endpoint) {
			t.Fatalf("unencrypted configuration: %v", err)
		}
		cfg, err = c.GetReportConfig(t.Context(), connect.NewRequest(&nagiv1.GetReportConfigRequest{}))
		b, _ := json.Marshal(cfg)
		if err != nil || !cfg.Msg.HasApiKey || strings.Contains(string(b), update.ApiKey) {
			t.Fatalf("read leaked credential: %v", err)
		}
		update.Enabled, update.ApiKey, update.AllowPrivate = false, "", true
		if cfg, err = c.UpdateReportConfig(t.Context(), connect.NewRequest(update)); err != nil || !cfg.Msg.HasApiKey {
			t.Fatalf("preserve: %v", err)
		}
		if cfg, err = c.GetReportConfig(t.Context(), connect.NewRequest(&nagiv1.GetReportConfigRequest{})); err != nil || !cfg.Msg.AllowPrivate {
			t.Fatalf("allow_private not stored: %v %v", cfg, err)
		}
		update.ClearApiKey = true
		if cfg, err = c.UpdateReportConfig(t.Context(), connect.NewRequest(update)); err != nil || cfg.Msg.HasApiKey {
			t.Fatalf("clear: %v", err)
		}
		// The audit DB is separate from panel.db; inspect through its public API.
		_, sc := h.login("admin", "report-test-password")
		for _, e := range h.list(sc, &nagiv1.AuditFilter{}) {
			if strings.Contains(e.Params, "not-a-real-key-unique-7481") {
				t.Fatal("key in audit")
			}
		}
	}
}

func TestReportMaterialExcludesRawLogsAndIdentifiers(t *testing.T) {
	h := newAuditHarness(t)
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	h.a.Logs = logs
	for _, category := range []string{logstore.Alert, logstore.Audit} {
		e := audit.Event{Category: audit.Alert, Action: "firing", ActorID: "panel", Params: `{"key":"cpu:private.example","title":"raw-log-body password=private-value"}`}
		if err := logs.Record(t.Context(), logstore.Event{Time: time.Now(), Category: category, Kind: "panel.firing", Data: e}); err != nil {
			t.Fatal(err)
		}
	}
	in, err := (&modelReportService{h.a}).reportInput(t.Context(), "en")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(in)
	// Existing archive redaction removes the key; never reconstruct it from title.
	if len(in.Alerts) != 1 || in.Alerts[0].Rule != "other" || in.Alerts[0].Validity != "unknown" {
		t.Fatalf("material: %s", b)
	}
	for _, forbidden := range []string{"private.example", "raw-log-body", "private-value", "params", "title"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("material contains %s", forbidden)
		}
	}
}
