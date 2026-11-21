package logstore_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// A slow recent-copy transaction (many nodes, slow disk) must not hold the
// machine journal: local audit/alert writes kept timing out in v0.37.0.
func TestSlowCopyReceiptDoesNotBlockLocalJournal(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := openStore(t, filepath.Join(dir, "logs.db"), 32<<20)
	c, err := s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	release := sync.OnceFunc(logstore.HoldCopies(c))
	defer release()
	received := make(chan error, 1)
	go func() {
		_, err := c.Receive(ctx, "n", logstore.CopyBatch{Source: "s", Category: logstore.Runtime, Through: 1,
			Events: []logstore.Event{{Seq: 1, Time: time.Now(), Category: logstore.Runtime, Kind: "out", Data: "x"}}})
		received <- err
	}()
	time.Sleep(200 * time.Millisecond) // receive is now inside a slow recent.db write
	wctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := s.Record(wctx, logstore.Event{Category: logstore.Audit, Kind: "while.copy.blocked"}); err != nil {
		t.Fatalf("local audit waited for a copy receipt: %v", err)
	}
	release()
	if err := <-received; err != nil {
		t.Fatalf("copy receipt did not resume after the slow writer: %v", err)
	}
}

// Ordinary overflow is counted as confirmed loss per category; important
// records use their own queue and are still committed.
func TestReportOverflowIsCountedAndImportantRecordsSurvive(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, 64<<20)
	blocker, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := range 12000 {
		s.Report(logstore.Event{Category: logstore.Access, Kind: "req", Data: map[string]any{"i": i, "path": "/api/x", "status": 200}})
	}
	s.Report(logstore.Event{Category: logstore.Audit, Kind: "must.keep"})
	// The writer is stuck behind the external lock for the whole loop, so a
	// Report that waited for a commit would add at least the 5 s busy timeout
	// for every batch (more than 20 batches). Redaction runs inline and is slow
	// under -race, so bound above that but far below the waits.
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("Report waited for the disk: %v", d)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(query(t, s, logstore.Filter{Category: logstore.Audit})) != 1 {
		t.Fatal("important record lost behind ordinary overflow")
	}
	kept := 0
	for after := int64(0); ; {
		rows := query(t, s, logstore.Filter{Category: logstore.Access, After: after, Limit: 1000})
		kept += len(rows)
		if len(rows) < 1000 {
			break
		}
		after = rows[len(rows)-1].Seq
	}
	var lost uint64
	for _, f := range s.WriteFailures() {
		if f.Category == logstore.Access {
			lost = f.Lost
			if f.LastError == "" || f.LastTime.IsZero() {
				t.Fatalf("failure without reason or time: %+v", f)
			}
		}
	}
	if lost == 0 || uint64(kept)+lost != 12000 {
		t.Fatalf("overflow not accounted: kept=%d lost=%d", kept, lost)
	}
}
