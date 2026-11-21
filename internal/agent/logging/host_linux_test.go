package logging

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Opt in only in an isolated system journal containing exactly fifteen fixture
// manager failures. Never run fixture assertions against a developer's host.
func TestHostSystemJournalIntegration(t *testing.T) {
	if os.Getenv("NAGI_TEST_HOST_JOURNAL") != "1" {
		t.Skip("requires isolated journal fixture")
	}
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		RunHost(ctx, logs, logstore.DefaultConfig())
		cancel()
		rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Runtime})
		if err != nil || len(rows) != 15 {
			status, _ := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
			for _, row := range status {
				t.Logf("%s: %s", row.Kind, row.Data)
			}
			t.Fatalf("full backlog/resume: %d rows %v", len(rows), err)
		}
		b, err := logs.Checkpoint(t.Context(), "host.journal")
		var cursor string
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor == "" {
			t.Fatalf("durable cursor: %s %v", b, err)
		}
		if err := (systemJournal{}).check(t.Context(), cursor); err != nil {
			t.Fatalf("cannot seek to committed cursor: %v", err)
		}
		var first struct {
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(rows[0].Data.(json.RawMessage), &first); err != nil {
			t.Fatal(err)
		}
		if err := (systemJournal{}).check(t.Context(), first.Cursor); err != nil {
			t.Fatalf("retained cursor from previous boot rejected: %v", err)
		}
	}
}

func TestHostSystemJournalAuthenticationIntegration(t *testing.T) {
	if os.Getenv("NAGI_TEST_HOST_JOURNAL") != "1" {
		t.Skip("requires isolated journal fixture with eight SSH failures and one sudo event")
	}
	for _, auth := range []bool{false, true} {
		logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		cfg := logstore.DefaultConfig()
		cfg.HostSSH, cfg.HostSudo, cfg.HostSSHFailuresPerMinute = auth, auth, 2
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		RunHost(ctx, logs, cfg)
		cancel()
		rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Security})
		if err != nil {
			t.Fatal(err)
		}
		details, count, sudo := 0, 0, 0
		for _, row := range rows {
			var data struct {
				Count int `json:"count"`
			}
			b := row.Data.(json.RawMessage)
			if err := json.Unmarshal(b, &data); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "arbitrary-private-value") {
				t.Fatal("sudo command leaked through real journal")
			}
			if row.Kind == "host.ssh.failed" {
				details++
			}
			if strings.HasPrefix(row.Kind, "host.ssh.failed") {
				count += data.Count
			}
			if row.Kind == "host.sudo" {
				sudo++
			}
		}
		logs.Close()
		if auth && (details != 2 || count != 8 || sudo != 1) {
			t.Fatalf("journal auth: details=%d failures=%d sudo=%d", details, count, sudo)
		}
		if !auth && len(rows) != 0 {
			t.Fatalf("disabled authentication collected: %d", len(rows))
		}
	}
}

func TestHostSystemJournalDeniedIntegration(t *testing.T) {
	if os.Getenv("NAGI_TEST_HOST_JOURNAL_DENIED") != "1" {
		t.Skip("requires isolated unreadable system journal and unprivileged user")
	}
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	RunHost(ctx, logs, logstore.DefaultConfig())
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
	if err != nil || len(rows) != 1 || rows[0].Kind != "host.journal.unavailable" {
		t.Fatalf("permission denial hidden: %+v %v", rows, err)
	}
}
