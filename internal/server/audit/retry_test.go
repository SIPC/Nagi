package audit

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	_ "modernc.org/sqlite"
)

// A stalled audit.db must not silently drop records: the timed-out record is
// counted as a failed attempt (not lost), retried in order after the stall, and
// the chain stays valid.
func TestStalledAuditInsertIsRetriedNotDropped(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	budget, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer budget.Close()
	if err := l.UseBudget(ctx, budget, false); err != nil {
		t.Fatal(err)
	}
	if l.Record(ctx, Event{Category: Operation, Action: "before"}) == 0 {
		t.Fatal("seed")
	}
	// Another connection holds the write lock, as a long fsync stall would.
	blocker, err := sql.Open("sqlite", "file:"+filepath.ToSlash(l.path)+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	conn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if l.Record(ctx, Event{Category: Security, Action: "stalled"}) != 0 {
		t.Fatal("insert should not commit while audit.db is locked")
	}
	// Queued behind the stalled one even though it would commit by itself.
	l.Record(ctx, Event{Category: Operation, Action: "behind"})
	f := failure(budget)
	if f.Failed == 0 || f.Lost != 0 || f.LastError == "" {
		t.Fatalf("stall should count as a failed attempt, not a loss: %+v", f)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var actions string
		if err := l.db.QueryRowContext(ctx, `SELECT group_concat(action, ',') FROM (SELECT action FROM events ORDER BY id)`).Scan(&actions); err != nil {
			t.Fatal(err)
		}
		if actions == "before,stalled,behind" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry did not commit in order: %q", actions)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v, err := l.Verify(ctx); err != nil || !v.OK() {
		t.Fatalf("chain broken by retry: %+v %v", v, err)
	}
	if f := failure(budget); f.Lost != 0 {
		t.Fatalf("retried record counted as lost: %+v", f)
	}
}

func failure(s *logstore.Store) logstore.WriteFailure {
	for _, f := range s.WriteFailures() {
		if f.Category == logstore.Audit {
			return f
		}
	}
	return logstore.WriteFailure{}
}
