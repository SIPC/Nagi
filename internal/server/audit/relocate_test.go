package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// legacyChain writes a realistic legacy chain at path: pruned records leave
// tombstones the chain has to bridge (with their audit_pruned records), a sink
// cursor, and values whose canonical JSON needs escaping.
func legacyChain(t *testing.T, path string, n int) (string, int64) {
	t.Helper()
	ctx := t.Context()
	l, err := Open(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := range n {
		e := Event{Category: Operation, Action: "Deploy", ActorName: "运维 \"admin\"", Params: `{"i":` + strings.Repeat("1", i%5+1) + `}`}
		if i < n/2 {
			// Old operations expire; old logins are kept longer and stay
			// between them, so the expired ones become tombstones.
			e.Time = time.Now().Add(-100 * 24 * time.Hour)
			if i%3 == 0 {
				e.Category = Login
			}
		}
		l.Record(ctx, e)
	}
	if err := l.Prune(ctx, DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := l.SetMeta(ctx, "sink:s1:cursor", "7"); err != nil {
		t.Fatal(err)
	}
	r, err := l.Verify(ctx)
	if err != nil || !r.OK() || r.Tombstones == 0 {
		t.Fatalf("legacy fixture: %+v %v", r, err)
	}
	return r.LastHash, r.Records
}

func journal(t *testing.T) *logstore.Store {
	t.Helper()
	s, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func kinds(t *testing.T, logs *logstore.Store, prefix string) []string {
	t.Helper()
	rows, err := logs.Query(t.Context(), logstore.Filter{KindPrefix: prefix, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Kind)
	}
	return out
}

// The moved chain is the same chain: every id, prev_hash, hash, tombstone and
// sink cursor survives, it verifies on its own, new records link to the old
// last hash, export matches, and the original is deleted once, with a log entry.
func TestRelocateKeepsWholeChainAndDeletesOriginalOnce(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	old := filepath.Join(dir, "panel", "audit.db")
	dst := filepath.Join(dir, "logs", "audit", "audit.db")
	lastHash, records := legacyChain(t, old, 400)
	before := exportAll(t, old)
	logs := journal(t)

	got, err := Relocate(ctx, old, dst, logs)
	if err != nil || got != dst {
		t.Fatalf("relocate: %s %v", got, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified original was not deleted")
	}
	r, err := VerifyFile(ctx, dst)
	if err != nil || !r.OK() || r.Records != records || r.LastHash != lastHash || r.Tombstones == 0 {
		t.Fatalf("moved chain does not verify on its own: %+v %v", r, err)
	}
	if after := exportAll(t, dst); !bytes.Equal(before, after) {
		t.Fatal("export changed by the move")
	}
	l, err := Open(ctx, dst, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if l.Meta(ctx, "sink:s1:cursor") != "7" {
		t.Fatal("sink cursor lost: shipping would resend or skip records")
	}
	id := l.Record(ctx, Event{Category: Operation, Action: "AfterMove"})
	e, err := l.Get(ctx, id)
	if err != nil || e.PrevHash != lastHash {
		t.Fatalf("new chain does not continue the old one: %+v %v", e, err)
	}
	if r, err := l.Verify(ctx); err != nil || !r.OK() {
		t.Fatalf("%+v %v", r, err)
	}
	l.Close()

	// A second start finds nothing to do and logs no second deletion.
	if got, err := Relocate(ctx, old, dst, logs); err != nil || got != dst {
		t.Fatalf("rerun: %s %v", got, err)
	}
	if k := kinds(t, logs, "legacy.cleanup.audit."); strings.Join(k, ",") != "legacy.cleanup.audit.moved" {
		t.Fatalf("deletion log: %v", k)
	}
}

// A tampered chain is never destroyed or laundered. Renamed on the same file
// system, its bytes are unchanged and the background check reports the tampered
// record; copied across file systems, the original stays and nothing is logged
// as moved or deleted.
func TestRelocateKeepsTamperedOriginal(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	tamper := func(path string) []byte {
		legacyChain(t, path, 40)
		d, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE events SET actor_name='mallory' WHERE id=(SELECT max(id)-3 FROM events)`); err != nil {
			t.Fatal(err)
		}
		_, _ = d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		d.Close()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	old := filepath.Join(dir, "audit.db")
	dst := filepath.Join(dir, "new", "audit.db")
	before := tamper(old)
	got, err := Relocate(ctx, old, dst, journal(t))
	if err != nil || got != dst {
		t.Fatalf("rename: %s %v", got, err)
	}
	if after, _ := os.ReadFile(dst); !bytes.Equal(before, after) {
		t.Fatal("rename changed the tampered file")
	}
	if err := VerifyMoved(ctx, dst); err == nil || !strings.Contains(err.Error(), "可能被修改过") {
		t.Fatalf("tampering must be reported after the move: %v", err)
	}

	cold := filepath.Join(dir, "c", "audit.db")
	cdst := filepath.Join(dir, "c-new", "audit.db")
	tamper(cold)
	if _, err := copyVerified(ctx, cold, cdst, func(ChainSummary) error { return nil }); err == nil || !strings.Contains(err.Error(), "可能被修改过") {
		t.Fatalf("copy must refuse a tampered chain: %v", err)
	}
	for _, p := range []string{cdst, cdst + ".tmp"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not exist", p)
		}
	}
	if _, err := os.Stat(cold); err != nil {
		t.Fatal("tampered original deleted")
	}
}

// A kill after the verified copy is in place but before the original is
// deleted: the next start uses the copy and finishes the deletion. A kill
// before the rename leaves only a temporary file, which is discarded.
func TestRelocateResumesAfterKill(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	old := filepath.Join(dir, "audit.db")
	dst := filepath.Join(dir, "new", "audit.db")
	lastHash, _ := legacyChain(t, old, 60)
	logs := journal(t)

	// Killed mid-copy: a partial temporary file.
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".tmp", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Killed after the rename, before the deletion.
	if _, err := copyVerified(ctx, old, dst, func(sum ChainSummary) error {
		return logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.audit.copied"},
			relocateCheckpoint(dst), relocateState{Phase: "copied", Digest: sum.Digest})
	}); err != nil {
		t.Fatal(err)
	}
	// The panel ran on the copy in between: new records and a moved cursor.
	l, err := Open(ctx, dst, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	l.Record(ctx, Event{Category: Operation, Action: "WhileHalfway"})
	_ = l.SetMeta(ctx, "sink:s1:cursor", "60")
	l.Close()

	got, err := Relocate(ctx, old, dst, logs)
	if err != nil || got != dst {
		t.Fatalf("resume: %s %v", got, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original kept after resume")
	}
	r, err := VerifyFile(ctx, dst)
	if err != nil || !r.OK() {
		t.Fatalf("%+v %v", r, err)
	}
	l, err = Open(ctx, dst, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	list, err := l.List(ctx, Filter{Limit: 1})
	if err != nil || list[0].Action != "WhileHalfway" || list[0].PrevHash != lastHash {
		t.Fatalf("records written on the copy lost: %+v %v", list, err)
	}
}

// An older version that ran after the move wrote a fresh chain at the old
// path. It is neither deleted unverified nor merged into the moved chain.
func TestRelocateRefusesOriginalWrittenAfterMove(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	old := filepath.Join(dir, "audit.db")
	dst := filepath.Join(dir, "new", "audit.db")
	legacyChain(t, old, 30)
	logs := journal(t)
	if _, err := Relocate(ctx, old, dst, logs); err != nil {
		t.Fatal(err)
	}
	legacyChain(t, old, 30) // the rolled-back version starts a new audit.db
	if _, err := Relocate(ctx, old, dst, logs); !errors.Is(err, ErrWrittenAfterMove) {
		t.Fatalf("must refuse: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("rolled-back chain deleted without its own verified copy")
	}
	name, err := RollbackName(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := RollbackName(ctx, old)
	if again != name || !strings.HasPrefix(name, "audit-rollback-") {
		t.Fatalf("rollback name must be stable: %s %s", name, again)
	}
	if _, err := Relocate(ctx, old, filepath.Join(dir, "new", name), logs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rolled-back chain not moved after verification")
	}
}

func exportAll(t *testing.T, path string) []byte {
	t.Helper()
	d, err := openRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	l := &Log{db: d}
	var out bytes.Buffer
	if err := l.Each(context.Background(), Filter{}, func(e Event) error {
		b, _ := json.Marshal(ExportLine(e))
		out.Write(append(b, '\n'))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
