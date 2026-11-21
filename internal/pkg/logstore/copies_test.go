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

func TestPanelReconciliationFillsHolesWithoutResurrectingPrunedCopies(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := openStore(t, filepath.Join(dir, "logs.db"), 32<<20)
	cfg := logstore.Config{RecentCapacityMiB: 16, RecentHours: 1}
	c, err := s.OpenCopies(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	at, received := time.Now().Add(-72*time.Hour).Truncate(time.Second), time.Now().Add(-time.Minute).Truncate(time.Second)
	event := func(seq int64) logstore.Event {
		return logstore.Event{Seq: seq, Source: "original-disk", Time: at, Received: received, Category: logstore.Audit, Kind: "operation", Data: fmt.Sprint(seq)}
	}
	// This receiver saw 2 and 4, but 3 had already been pruned at the node.
	if _, err := c.Receive(ctx, "node", logstore.CopyBatch{Source: "original-disk", Category: logstore.Audit, Through: 8, Events: []logstore.Event{event(2), event(4)}, Pruned: []logstore.Pruned{{Category: logstore.Audit, First: 3, Last: 3, Count: 1}}}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	d, err := db.Open(filepath.Join(dir, "recent.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx, `UPDATE log_events SET received_ns=?`, time.Now().Add(-2*time.Hour).UnixNano())
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.OpenCopies(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Local pruning covers the envelope [2,4], but only 2 and 4 were deleted.
	// A peer still has 3: do not confuse a genuine hole with local deletion.
	for _, seq := range []int64{2, 3, 4} {
		item := logstore.CopyItem{Node: "node", Event: event(seq)}
		needed, err := c.Needs(ctx, item.Index())
		if err != nil || needed != (seq == 3) {
			t.Fatalf("needs %d: %v %v", seq, needed, err)
		}
		if err := c.Merge(ctx, "node", logstore.CopyBatch{Source: item.Event.Source, Category: logstore.Audit, Through: seq, Events: []logstore.Event{item.Event}}); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := c.Read(ctx, "node", "original-disk", logstore.Filter{Category: logstore.Audit})
	if err != nil || len(p.Events) != 1 || p.Events[0].Seq != 3 || !p.Events[0].Received.Equal(event(3).Received) {
		t.Fatalf("hole/expiry/first receipt: %+v %v", p, err)
	}
	if r, err := c.Receipt(ctx, "node", "original-disk", logstore.Audit); err != nil || r.After != 8 {
		t.Fatalf("peer moved node receipt: %+v %v", r, err)
	}
	// A peer may be ahead of this panel's direct stream. Reconnect must still
	// accept the real node's intervening events and deduplicate its overlap.
	ahead := event(11)
	if err := c.Merge(ctx, "node", logstore.CopyBatch{Source: ahead.Source, Category: ahead.Category, Through: 11, Events: []logstore.Event{ahead}}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	d, err = db.Open(filepath.Join(dir, "recent.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx, `UPDATE log_events SET received_ns=? WHERE seq IN (SELECT local_seq FROM copy_origins WHERE remote_seq=11)`, time.Now().Add(-2*time.Hour).UnixNano())
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.OpenCopies(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	e := event(10)
	if err := c.Merge(ctx, "node", logstore.CopyBatch{Source: e.Source, Category: e.Category, Through: 10, Events: []logstore.Event{e}}); err != nil {
		t.Fatal(err)
	}
	if r, err := c.Receipt(ctx, "node", e.Source, e.Category); err != nil || r.After != 8 {
		t.Fatalf("future peer copy acknowledged missing node data: %+v %v", r, err)
	}
	if _, err := c.Receive(ctx, "node", logstore.CopyBatch{Source: e.Source, Category: e.Category, After: 8, Through: 12, Events: []logstore.Event{event(9), e, ahead, event(12)}}); err != nil {
		t.Fatal(err)
	}
	p, _, err = c.Read(ctx, "node", e.Source, logstore.Filter{Category: e.Category})
	if err != nil || len(p.Events) != 4 || p.Events[0].Seq != 3 || p.Events[1].Seq != 9 || p.Events[2].Seq != 10 || p.Events[3].Seq != 12 || p.Next != 12 {
		t.Fatalf("node reconnection lost original identities: %+v %v", p, err)
	}
	e.Data = "conflicting body"
	if err := c.Merge(ctx, "node", logstore.CopyBatch{Source: e.Source, Category: e.Category, Through: 10, Events: []logstore.Event{e}}); !errors.Is(err, logstore.ErrCopyConflict) {
		t.Fatalf("peer overwrite accepted: %v", err)
	}
	if err := c.SaveSyncPosition(ctx, "peer", logstore.CopyPosition{Source: "peer-disk", After: 123}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = s.OpenCopies(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if pos, err := c.SyncPosition(ctx, "peer"); err != nil || pos.Source != "peer-disk" || pos.After != 123 {
		t.Fatalf("lost durable peer position: %+v %v", pos, err)
	}
	if needed, err := c.Needs(ctx, (logstore.CopyItem{Node: "node", Event: event(2)}).Index()); err != nil || needed {
		t.Fatalf("restart lost local deletion: %v %v", needed, err)
	}
}
