package nodes

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReportedAlertsPersistHandlingAndRecoveryOutsideBusinessDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var business *sql.DB
	p := newSessionPanelDB(t, func(d store.DBTX) store.DBTX { business = d.(*sql.DB); return d }, false)
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(ctx, path, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { logs.Close() }()
	p.r.AlertLogs = logs
	s := p.connect(t)
	defer s.sess.Close()
	source := &nagiv1.Alert{Key: "disk", Source: "source-one", Occurrence: "occurrence-secret-marker", Revision: 1, Status: "firing", Validity: "valid", Title: "Disk full", Message: "password=super-private-value", Since: timestamppb.Now()}
	report := func(rev uint64, current, history []*nagiv1.Alert) *nagiv1.ReportResponse {
		t.Helper()
		res, err := s.panel.Report(ctx, &nagiv1.ReportRequest{Alerts: &nagiv1.AlertSync{Source: source.Source, Revision: rev, Complete: true, Current: current, History: history}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := report(1, []*nagiv1.Alert{source}, nil)
	if len(res.AlertHandling) != 1 || res.AlertHandling[0].Alert.Key != "disk" {
		t.Fatalf("report snapshot not connected: %v", res)
	}
	if strings.Contains(res.AlertHandling[0].Alert.Message, "super-private-value") {
		t.Fatal("snapshot leaked credential")
	}
	req := &nagiv1.HandleAlertRequest{NodeId: p.nodeID, Key: source.Key, Occurrence: source.Occurrence, Action: "acknowledge"}
	a, err := p.r.HandleAlert(ctx, req, "user:42")
	if err != nil || !a.GetAcknowledged() || a.Alert.Status != "firing" {
		t.Fatalf("ack manufactured recovery: %v %v", a, err)
	}
	if _, err := p.r.HandleAlert(ctx, req, "user:43"); status.Code(err) != codes.Aborted {
		t.Fatalf("lost-update guard: %v", err)
	}
	req.Action, req.HandlingRevision, req.Conclusion = "conclusion", 1, "investigating password=another-private-value"
	if _, err := p.r.HandleAlert(ctx, req, "user:42"); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(ctx, path, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	p.r.AlertLogs = logs
	res = report(1, []*nagiv1.Alert{source}, nil)
	if len(res.AlertHandling) != 1 || !res.AlertHandling[0].Acknowledged || res.AlertHandling[0].HandlingRevision != 2 || strings.Contains(res.AlertHandling[0].Conclusion, "another-private-value") {
		t.Fatalf("restart lost or exposed handling: %v", res)
	}
	terminal := proto.CloneOf(source)
	terminal.Status, terminal.EndReason, terminal.Revision = "resolved", "recovered", 2
	report(2, nil, []*nagiv1.Alert{terminal})
	report(1, []*nagiv1.Alert{source}, nil) // old reports cannot resurrect a recovered occurrence
	states, err := p.r.AlertStates(ctx, p.nodeID, true)
	if err != nil || len(states) != 1 || states[0].Alert.Status != "resolved" || !states[0].Acknowledged {
		t.Fatalf("recovery/ordering: %v %v", states, err)
	}
	// 基线进度：旧节点（不带 baselines_supported）算没上报过；新节点的完整快照整组替换，存在日志库里。
	if _, _, reported, err := p.r.AlertBaselines(ctx, p.nodeID); err != nil || reported {
		t.Fatalf("v0.37 node must be unsupported: %v %v", reported, err)
	}
	baseline := func(rev uint64, list ...*nagiv1.AlertBaseline) {
		t.Helper()
		if _, err := s.panel.Report(ctx, &nagiv1.ReportRequest{Alerts: &nagiv1.AlertSync{Source: source.Source, Revision: rev, Complete: true,
			BaselinesSupported: true, Baselines: list}}); err != nil {
			t.Fatal(err)
		}
	}
	baseline(3, &nagiv1.AlertBaseline{Rule: alert.RuleL4Surge, ProjectId: "p1", Subject: "tcp", RequiredSeconds: 21600, CollectedSeconds: 9000, Status: "building"},
		&nagiv1.AlertBaseline{Rule: alert.RuleTraffic, ProjectId: "p1", Subject: "web", Status: "ready"})
	baseline(4, &nagiv1.AlertBaseline{Rule: alert.RuleL4Surge, ProjectId: "p1", Subject: "tcp", RequiredSeconds: 21600, CollectedSeconds: 21600, Status: "ready"})
	if list, _, reported, err := p.r.AlertBaselines(ctx, p.nodeID); err != nil || !reported || len(list) != 1 || list[0].GetStatus() != "ready" {
		t.Fatalf("baselines must be replaced by the latest snapshot: %v %v %v", list, reported, err)
	}
	events, err := logs.Query(ctx, logstore.Filter{Category: logstore.Alert, KindPrefix: "alert.handled", Limit: 100})
	if err != nil || len(events) != 2 {
		t.Fatalf("handling history: %v %v", events, err)
	}
	rows, err := business.QueryContext(ctx, `SELECT value FROM settings UNION ALL SELECT COALESCE(CAST(info AS TEXT),'') FROM nodes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(v, source.Occurrence) || strings.Contains(v, "investigating") {
			t.Fatal("alert history written to business DB")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := business.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('alert_node_overrides', 'alert_project_overrides', 'alert_container_overrides', 'alert_route_overrides') AND (name LIKE '%alert%' OR name GLOB 'log_*' OR name GLOB 'logs*' OR name LIKE '%audit%')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("log tables in business DB: %d %v", count, err)
	}
}

func TestPanelAlertHandlingPersistsAndMuteDoesNotResolve(t *testing.T) {
	p := newSessionPanel(t)
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	p.r.AlertLogs = logs
	firing := &nagiv1.Alert{Key: "offline:n1", Status: "firing", Title: "Node offline"}
	if send, err := p.r.trackPanelAlert(ctx, firing); err != nil || !send {
		t.Fatal(send, err)
	}
	states, err := p.r.AlertStates(ctx, "", false)
	if err != nil || len(states) != 1 {
		t.Fatal(states, err)
	}
	a := states[0]
	_, err = p.r.HandleAlert(ctx, &nagiv1.HandleAlertRequest{Key: a.Alert.Key, Occurrence: a.Alert.Occurrence, Action: "silence", Until: timestamppb.New(time.Now().Add(time.Hour))}, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	if send, err := p.r.trackPanelAlert(ctx, firing); err != nil || send {
		t.Fatalf("mute ignored: %v %v", send, err)
	}
	if _, err := p.r.HandleAlert(ctx, &nagiv1.HandleAlertRequest{Key: a.Alert.Key, Occurrence: a.Alert.Occurrence, HandlingRevision: 1, Action: "end"}, "user:1"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("continuous detection manually hidden: %v", err)
	}
	ended := proto.CloneOf(firing)
	ended.Status = "resolved"
	if send, err := p.r.trackPanelAlert(ctx, ended); err != nil || !send {
		t.Fatalf("mute suppressed recovery: %v %v", send, err)
	}
	states, err = p.r.AlertStates(ctx, "", true)
	if err != nil || len(states) != 1 || states[0].Alert.EndReason != "recovered" || states[0].Alert.Occurrence != a.Alert.Occurrence {
		t.Fatal(states, err)
	}
	if _, err := p.r.trackPanelAlert(ctx, &nagiv1.Alert{Key: "security:event", Status: "firing", Title: "Security event"}); err != nil {
		t.Fatal(err)
	}
	states, err = p.r.AlertStates(ctx, "", false)
	if err != nil || len(states) != 1 {
		t.Fatal(states, err)
	}
	a = states[0]
	manual, err := p.r.HandleAlert(ctx, &nagiv1.HandleAlertRequest{Key: a.Alert.Key, Occurrence: a.Alert.Occurrence, Action: "end"}, "user:1")
	if err != nil || manual.Alert.Status != "ended" || manual.Alert.EndReason != "manual" {
		t.Fatalf("manual ending must not manufacture recovery: %v %v", manual, err)
	}
	states, err = p.r.AlertStates(ctx, "", false)
	if err != nil || len(states) != 0 {
		t.Fatalf("ended event still current: %v %v", states, err)
	}
}
