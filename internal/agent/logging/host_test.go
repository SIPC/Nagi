package logging

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

type journalFixture struct {
	entries      []journalEntry
	checkErr     error
	cancel       context.CancelFunc
	seenPosition string
}

func (s *journalFixture) check(_ context.Context, after string) error {
	s.seenPosition = after
	return s.checkErr
}

func (s *journalFixture) follow(ctx context.Context, after string, _ time.Time, emit func(journalEntry) error) error {
	ready := after == ""
	for _, e := range s.entries {
		if !ready {
			ready = e.text("__CURSOR") == after
			continue
		}
		if err := emit(e); err != nil {
			s.cancel()
			return err
		}
	}
	s.cancel()
	return ctx.Err()
}

func hostLine(cursor, unit, message string, extra map[string]string) journalEntry {
	values := map[string]string{"__CURSOR": cursor, "__REALTIME_TIMESTAMP": "1795212345678901", "_SYSTEMD_UNIT": unit, "MESSAGE": message, "PRIORITY": "6"}
	for k, v := range extra {
		values[k] = v
	}
	e := journalEntry{}
	for k, v := range values {
		e[k], _ = json.Marshal(v)
	}
	return e
}

func TestHostHistorySelectionRedactionAndDurableResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	logs, err := logstore.Open(t.Context(), path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { logs.Close() }()
	kernel := map[string]string{"_TRANSPORT": "kernel"}
	manager := map[string]string{"_PID": "1", "UNIT": "docker.service"}
	entries := []journalEntry{
		hostLine("1", "docker.service", "normal info", nil),
		hostLine("2", "docker.service", "API failed token=do-not-store", map[string]string{"PRIORITY": "3"}),
		hostLine("3", "containerd.service", "snapshot failed", map[string]string{"PRIORITY": "4"}),
		hostLine("4", "", "Out of memory: Killed process 321", kernel),
		hostLine("5", "", "EXT4-fs error: remounting filesystem read-only", kernel),
		hostLine("6", "", "eth0: Link is Down", kernel),
		hostLine("7", "init.scope", "Started Docker", manager),
		hostLine("8", "init.scope", "Failed to start unrelated.service", map[string]string{"_PID": "1", "UNIT": "unrelated.service"}),
		hostLine("9", "init.scope", "Startup finished in 2s", manager),
		hostLine("10", "systemd-resolved.service", "DNS server unreachable", nil),
		hostLine("11", "chronyd.service", "System clock change detected", nil),
		hostLine("12", "systemd-journald.service", "Suppressed 423 messages", nil),
		hostLine("13", "nagi-agent.service", "panic: unhandled failure", nil),
		hostLine("14", "nagi-server.service", `time=2026-11-21T01:00:00Z level=ERROR msg="already persisted"`, nil),
		hostLine("15", "nagi-agent.service", `{"time":"2026-11-21","level":"INFO","msg":"already persisted"}`, nil),
		hostLine("16", "nagi-agent.service", "time=2026-11-21 level=ERROR msg=本地日志入库失败", nil),
		hostLine("17", "ssh.service", "Failed password for root", nil),
		hostLine("18", "my-app.service", "business output", map[string]string{"PRIORITY": "3"}),
		hostLine("19", "", "[UFW BLOCK] SRC=192.0.2.1", kernel),
		hostLine("20", "nagi-agent.service", strings.Repeat("x", 9000), nil),
	}
	run := func(entries []journalEntry) string {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := &journalFixture{entries: entries, cancel: cancel}
		runHost(ctx, logs, logstore.DefaultConfig(), s)
		return s.seenPosition
	}
	run(entries)
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = logstore.Open(t.Context(), path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, hostLine("21", "docker.service", "new warning", map[string]string{"PRIORITY": "4"}))
	if after := run(entries); after != "20" {
		t.Fatalf("resume=%q", after)
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"host.daemon.warning", "host.daemon.warning", "host.kernel.memory", "host.kernel.storage", "host.kernel.network", "host.service.state", "host.service.failed", "host.host.boot", "host.service.warning", "host.service.warning", "host.service.warning", "host.nagi.output", "host.nagi.output", "host.nagi.output", "host.daemon.warning"}
	if len(rows) != len(want) {
		t.Fatalf("rows=%d want=%d: %+v", len(rows), len(want), rows)
	}
	for i, r := range rows {
		if r.Kind != want[i] {
			t.Errorf("row %d kind=%s want=%s", i, r.Kind, want[i])
		}
		if strings.Contains(string(r.Data.(json.RawMessage)), "do-not-store") {
			t.Fatal("secret in host history")
		}
	}
	if !rows[0].Time.Equal(time.UnixMicro(1795212345678901)) {
		t.Fatal("source time lost")
	}
	if !strings.Contains(string(rows[13].Data.(json.RawMessage)), `"truncated":true`) {
		t.Fatal("long message silently clipped")
	}
}

func TestHostUnavailableAndRotatedCursorAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"permission", errors.New("insufficient journal permissions"), "host.journal.unavailable"},
		{"rotated", errJournalCursor, "host.journal.gap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			if err := logs.RecordCheckpoint(t.Context(), logstore.Event{Category: logstore.Runtime, Kind: "before"}, "host.journal", "old"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			src := &journalFixture{cancel: cancel, checkErr: tc.err, entries: []journalEntry{hostLine("new", "docker.service", "warning", map[string]string{"PRIORITY": "4"})}}
			runHost(ctx, logs, logstore.DefaultConfig(), src)
			rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
			if err != nil || len(rows) == 0 || rows[0].Kind != tc.kind {
				t.Fatalf("missing availability: %+v %v", rows, err)
			}
			b, err := logs.Checkpoint(t.Context(), "host.journal")
			want := `"old"`
			if tc.name == "rotated" {
				want = `"new"`
			}
			if err != nil || string(b) != want {
				t.Fatalf("checkpoint=%s want=%s err=%v", b, want, err)
			}
		})
	}
}
