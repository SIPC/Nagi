package auditlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// writeLegacy writes v0.37.0-style JSON Lines with seq from..to into dir,
// rotated every 40 lines like the old writer, and returns the raw lines.
func writeLegacy(t *testing.T, dir string, from, to int64) [][]byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var all, cur [][]byte
	flush := func(name string) {
		if len(cur) == 0 {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(bytes.Join(cur, []byte("\n")), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		cur = nil
	}
	for s := from; s <= to; s++ {
		// Field order and escaping exactly as the old json.Marshal wrote them.
		b, _ := json.Marshal(Event{Seq: s, Time: time.Unix(1700000000+s, 123456789).UTC(), Panel: "sha256//p", Actor: "运维 <admin>",
			Action: "DeployContainer", Target: "p/web", Detail: "password=[REDACTED:password]", Result: "ok"})
		all, cur = append(all, b), append(cur, b)
		if len(cur) == 40 && s != to {
			flush(fmt.Sprintf("audit.%020d.jsonl", s))
		}
	}
	flush(current)
	return all
}

func journalStore(t *testing.T) *logstore.Store {
	t.Helper()
	s, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The legacy lines are imported unchanged, with their numbers: the panel's
// shipping cursor picks up exactly where it was (no resend, no gap), the chain
// verifies on its own, new records continue it, and a restart imports nothing twice.
func TestLegacyImportKeepsLinesAndShippingCursor(t *testing.T) {
	dir := t.TempDir()
	legacy := writeLegacy(t, filepath.Join(dir, "audit"), 1, 137)
	l := openTest(t, dir)
	// Panel shipped up to 100 before the upgrade.
	// The next record after the legacy lines is the chained import record.
	lines, first, last := l.Read(100, 500)
	if first != 101 || last != 138 || len(lines) != 38 {
		t.Fatalf("cursor continuity: %d-%d %d", first, last, len(lines))
	}
	for i, line := range lines[:37] {
		if !bytes.Equal(line, legacy[100+i]) {
			t.Fatalf("line %d changed by the import:\n%s\n%s", 101+i, line, legacy[100+i])
		}
	}
	if s := l.Record(Event{Action: "after"}); s != 139 { // 138 is the import record
		t.Fatalf("new numbering: %d", s)
	}
	l.Close()
	l = openTest(t, dir)
	r, err := l.Verify(t.Context())
	if err != nil || !r.OK() || r.Records != 139 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Old files lost to the old 10-file rotation become tombstones listed by the
// import record, so the chain verifies and the loss stays visible.
func TestLegacyImportRecordsRotationGaps(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, filepath.Join(dir, "audit"), 1, 120)
	for _, name := range rotated(filepath.Join(dir, "audit"))[:1] {
		if err := os.Remove(filepath.Join(dir, "audit", name)); err != nil {
			t.Fatal(err)
		}
	}
	// Remove a middle file too, as the old budget trim could.
	names := rotated(filepath.Join(dir, "audit"))
	if err := os.Remove(filepath.Join(dir, "audit", names[0])); err != nil {
		t.Fatal(err)
	}
	l := openTest(t, dir)
	r, err := l.Verify(t.Context())
	if err != nil || !r.OK() || r.Tombstones != 0 {
		// Leading loss: nothing to bridge, so no tombstones are needed.
		t.Fatalf("%+v %v", r, err)
	}
	dir2 := t.TempDir()
	writeLegacy(t, filepath.Join(dir2, "audit"), 1, 120)
	if err := os.Remove(filepath.Join(dir2, "audit", rotated(filepath.Join(dir2, "audit"))[1])); err != nil {
		t.Fatal(err)
	}
	l2 := openTest(t, dir2)
	r, err = l2.Verify(t.Context())
	if err != nil || !r.OK() || r.Tombstones != 40 {
		t.Fatalf("middle gap must be bridged by listed tombstones: %+v %v", r, err)
	}
	lines, _, _ := l2.After(0, 500)
	var imp Event
	_ = json.Unmarshal(lines[len(lines)-1], &imp)
	if imp.Action != legacyImport || !strings.Contains(imp.Detail, `"gaps":["41-80"]`) {
		t.Fatalf("import record must name the gap: %s", lines[len(lines)-1])
	}
}

// A corrupt line stops the import; new records keep going to the legacy file
// so nothing is lost or reordered, and after repair everything is imported.
func TestLegacyImportStopsAtCorruptionAndResumes(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "audit")
	writeLegacy(t, legacyDir, 1, 30)
	path := filepath.Join(legacyDir, current)
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(append([]byte(nil), good...), []byte("{corrupt}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "logs", "audit", "node-audit.db")
	l, err := Open(store, legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ImportLegacy(t.Context()); err == nil {
		t.Fatal("corrupt source was silently skipped")
	}
	if s := l.Record(Event{Action: "while-pending"}); s != 31 {
		t.Fatalf("pending write numbering: %d", s)
	}
	if lines, _, _ := l.Read(0, 500); len(lines) != 0 {
		t.Fatal("nothing may be served while the import is pending: the panel cursor must not move")
	}
	l.Close()
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte("{corrupt}\n"), nil, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	l = openTest(t, dir)
	lines, _, last := l.Read(0, 500)
	var e Event
	_ = json.Unmarshal(lines[30], &e)
	if len(lines) != 32 || e.Seq != 31 || e.Action != "while-pending" || last != 32 {
		t.Fatalf("resume lost or reordered records: %d lines, last %d, %+v", len(lines), last, e)
	}
}

// Deletion happens only after verification, and is logged once. A mismatch
// keeps the files.
func TestCleanupLegacyOnlyAfterVerification(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "audit")
	writeLegacy(t, legacyDir, 1, 90)
	logs := journalStore(t)
	l := openTest(t, dir)

	// A legacy line edited after the import no longer matches the store.
	path := filepath.Join(legacyDir, current)
	b, _ := os.ReadFile(path)
	edited := bytes.Replace(b, []byte(`"seq":85,`), []byte(`"seq":85,"x":1,`), 1)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.CleanupLegacy(t.Context(), logs); err == nil || len(files(legacyDir)) == 0 {
		t.Fatalf("mismatching files deleted: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.CleanupLegacy(t.Context(), logs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy directory left behind")
	}
	if err := l.CleanupLegacy(t.Context(), logs); err != nil {
		t.Fatal(err)
	}
	rows, _ := logs.Query(t.Context(), logstore.Filter{KindPrefix: "legacy.cleanup.node.audit.", Limit: 10})
	if len(rows) != 2 || rows[0].Kind != "legacy.cleanup.node.audit.deleting" || rows[1].Kind != "legacy.cleanup.node.audit.done" {
		t.Fatalf("deletion must be logged exactly once: %+v", rows)
	}
	if r, err := l.Verify(t.Context()); err != nil || !r.OK() || r.Records != 91 {
		t.Fatalf("store must stand alone after deletion: %+v %v", r, err)
	}
}

// A kill in the middle of deleting leaves some files; the next run verifies
// what is left and finishes. Lines written by a rolled-back older version are
// carried into the chain unchanged before they are deleted.
func TestCleanupLegacyResumesAndCarriesRollbackLines(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "audit")
	writeLegacy(t, legacyDir, 1, 90)
	logs := journalStore(t)
	l := openTest(t, dir)
	if err := os.Remove(filepath.Join(legacyDir, rotated(legacyDir)[0])); err != nil { // killed after one removal
		t.Fatal(err)
	}
	if err := l.CleanupLegacy(t.Context(), logs); err != nil {
		t.Fatal(err)
	}
	l.Record(Event{Action: "new"})
	l.Close()
	// An older version ran and wrote two lines with its own numbering.
	writeLegacy(t, legacyDir, 91, 92)
	rollback, _ := os.ReadFile(filepath.Join(legacyDir, current))
	l = openTest(t, dir)
	if err := l.CleanupLegacy(t.Context(), logs); err != nil {
		t.Fatal(err)
	}
	lines, _, _ := l.After(0, 1000)
	var carried int
	for _, line := range lines {
		var e Event
		_ = json.Unmarshal(line, &e)
		if e.Action == legacyLine && bytes.Contains(rollback, []byte(e.Detail)) {
			carried++
		}
	}
	if carried != 2 {
		t.Fatalf("rollback lines not carried unchanged: %d", carried)
	}
	if r, err := l.Verify(t.Context()); err != nil || !r.OK() {
		t.Fatalf("%+v %v", r, err)
	}
}

// The unified-log copy is resumable, skips chain bookkeeping records, and
// records sequences missing from the store as an explicit gap.
func TestHistoryCopyResumesWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, filepath.Join(dir, "audit"), 1, 70)
	// The oldest rotated file (1-40) was deleted by the old version.
	if err := os.Remove(filepath.Join(dir, "audit", rotated(filepath.Join(dir, "audit"))[0])); err != nil {
		t.Fatal(err)
	}
	l := openTest(t, dir)
	dest := journalStore(t)
	through := l.Seq()
	for range 2 {
		if err := l.MigrateHistory(t.Context(), dest, through); err != nil {
			t.Fatal(err)
		}
	}
	if !HistoryDone(t.Context(), dest, through) {
		t.Fatal("not marked done")
	}
	rows, err := dest.Query(t.Context(), logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 30 {
		t.Fatalf("lost or duplicated: %d %v", len(rows), err)
	}
	gaps, err := dest.Query(t.Context(), logstore.Filter{KindPrefix: "legacy.audit.gap", Limit: 10})
	if err != nil || len(gaps) != 1 || !strings.Contains(string(gaps[0].Data.(json.RawMessage)), `"to":40`) {
		t.Fatalf("missing sequences must be recorded once as a gap: %+v %v", gaps, err)
	}
	var e Event
	if err := json.Unmarshal(rows[0].Data.(json.RawMessage), &e); err != nil || e.Detail != "[已隐藏]" {
		t.Fatalf("copy bypassed redaction: %+v %v", e, err)
	}
}
