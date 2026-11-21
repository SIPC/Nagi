package logstore_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestRecentCopiesKeepDurableIndependentReceipts(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "logs.db")
	s := openStore(t, path, 32<<20)
	c, err := s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	e := logstore.Event{Seq: 3, Time: time.Now().Add(-72 * time.Hour), Category: logstore.Audit, Kind: "operation", Data: map[string]any{"token": "never-retain", "result": "original"}}
	b := logstore.CopyBatch{Source: "disk-a", Category: logstore.Audit, Through: 8, Events: []logstore.Event{e}, Pruned: []logstore.Pruned{{Category: logstore.Audit, First: 1, Last: 2, Count: 2}}, WriteErrors: 7}
	if r, err := c.Receive(ctx, "node-a", b); err != nil || r.After != 8 {
		t.Fatalf("commit: %+v %v", r, err)
	}
	// The old event clock cannot immediately expire newly received evidence.
	p, sourcePruned, err := c.Read(ctx, "node-a", "", logstore.Filter{Category: logstore.Audit})
	if err != nil || len(p.Events) != 1 || p.Next != 8 || p.State.WriteErrors != 7 || len(sourcePruned) != 1 {
		t.Fatalf("copy: %+v %+v %v", p, sourcePruned, err)
	}
	data, _ := json.Marshal(p.Events[0].Data)
	if strings.Contains(string(data), "never-retain") || !strings.Contains(string(data), "original") {
		t.Fatalf("redaction: %s", data)
	}
	if _, err := c.Receive(ctx, "node-a", b); err != nil {
		t.Fatal(err)
	}
	b.Events[0].Data = map[string]any{"result": "forged replay"}
	if _, err := c.Receive(ctx, "node-a", b); !errors.Is(err, logstore.ErrCopyConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = c.Read(ctx, "node-a", "disk-a", logstore.Filter{Category: logstore.Audit})
	if err != nil || len(p.Events) != 1 {
		t.Fatalf("restart: %+v %v", p, err)
	}
	data, _ = json.Marshal(p.Events[0].Data)
	if err != nil || len(p.Events) != 1 || strings.Contains(string(data), "forged") {
		t.Fatalf("restart/replay: %+v %s %v", p, data, err)
	}
	for _, q := range []struct{ node, source, category string }{{"node-b", "disk-a", logstore.Audit}, {"node-a", "disk-b", logstore.Audit}, {"node-a", "disk-a", logstore.Container}} {
		if r, err := c.Receipt(ctx, q.node, q.source, q.category); err != nil || r.After != 0 {
			t.Fatalf("cursor isolation: %+v %v", r, err)
		}
	}
	if _, err := c.Receive(ctx, "node-a", logstore.CopyBatch{Source: "disk-a", Category: logstore.Audit, After: 7, Through: 9}); !errors.Is(err, logstore.ErrCopyCursor) {
		t.Fatalf("stale cursor accepted: %v", err)
	}
	if _, _, err := c.Read(ctx, "node-a", "disk-a", logstore.Filter{Category: logstore.Audit, After: 9}); err == nil {
		t.Fatal("cursor beyond durable position accepted")
	}
	if _, _, err := c.Read(ctx, "node-a", "disk-a", logstore.Filter{}); err == nil {
		t.Fatal("independently advancing categories must not share a cursor")
	}
}

func TestRecentCopiesSharePhysicalBudgetAndPreserveAudit(t *testing.T) {
	for _, budget := range []int64{8 << 20, 32 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "logs.db")
			s := openStore(t, path, budget)
			c, err := s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 24})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Receive(ctx, "n", logstore.CopyBatch{Source: "s", Category: logstore.Audit, Through: 1, Events: []logstore.Event{{Seq: 1, Time: time.Now(), Category: logstore.Audit, Kind: "evidence", Data: "keep-audit"}}}); err != nil {
				t.Fatal(err)
			}
			var after int64
			for i := int64(2); i <= 301; i++ {
				if _, err := c.Receive(ctx, "n", logstore.CopyBatch{Source: "s", Category: logstore.Runtime, After: after, Through: i, Events: []logstore.Event{{Seq: i, Time: time.Now(), Category: logstore.Runtime, Kind: "output", Data: fmt.Sprint(i) + strings.Repeat("x", 48<<10)}}}); err != nil {
					t.Fatalf("receive %d: %v", i, err)
				}
				after = i
			}
			p, _, err := c.Read(ctx, "n", "s", logstore.Filter{Category: logstore.Runtime})
			if err != nil || len(p.State.Pruned) == 0 || p.State.Pruned[0].Count == 0 {
				t.Fatalf("shared budget did not prune copies: %+v %v", p.State, err)
			}
			a, _, err := c.Read(ctx, "n", "s", logstore.Filter{Category: logstore.Audit})
			if err != nil || len(a.Events) != 1 {
				t.Fatalf("ordinary output displaced audit: %+v %v", a, err)
			}
			var physical int64
			for _, p := range []string{path, filepath.Join(filepath.Dir(path), "recent.db")} {
				n, err := logstore.FileBytes(p)
				if err != nil {
					t.Fatal(err)
				}
				physical += n
			}
			if physical > budget {
				t.Fatalf("shared physical quota exceeded: %d", physical)
			}
			if n, err := logstore.FileBytes(filepath.Join(filepath.Dir(path), "recent.db")); err != nil || n > 16<<20 {
				t.Fatalf("copy sub-budget: %d %v", n, err)
			}
		})
	}
}

func TestRecentCopyExpiryDoesNotResetAcknowledgement(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := openStore(t, filepath.Join(dir, "logs.db"), 32<<20)
	c, err := s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	batch := logstore.CopyBatch{Source: "source", Category: logstore.Audit, Through: 4, Events: []logstore.Event{{Seq: 4, Time: time.Now().Add(7 * 24 * time.Hour), Category: logstore.Audit, Kind: "future-node-clock", Data: "evidence"}}}
	if _, err := c.Receive(ctx, "node", batch); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Offline fixture: advance the age of the panel receipt, not the untrusted
	// node clock. Query through the public reader after reopening.
	d, err := db.Open(filepath.Join(dir, "recent.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx, `UPDATE log_events SET received_ns=?`, time.Now().Add(-2*time.Hour).UnixNano())
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.OpenCopies(ctx, logstore.Config{RecentCapacityMiB: 16, RecentHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, _, err := c.Read(ctx, "node", "source", logstore.Filter{Category: logstore.Audit})
	if err != nil || len(p.Events) != 0 || p.Next != 4 || len(p.State.Pruned) != 1 || p.State.Pruned[0].Count != 1 {
		t.Fatalf("expiry: %+v %v", p, err)
	}
	if r, err := c.Receive(ctx, "node", batch); err != nil || r.After != 4 {
		t.Fatalf("late replay: %+v %v", r, err)
	}
	p, _, err = c.Read(ctx, "node", "source", logstore.Filter{Category: logstore.Audit})
	if err != nil || len(p.Events) != 0 {
		t.Fatalf("expired evidence resurrected: %+v %v", p, err)
	}
}
