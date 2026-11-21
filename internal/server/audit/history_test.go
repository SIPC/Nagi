package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestArchiveImportIsReadOnlyResumableAndRenameStable(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	for i := range 137 {
		l.Record(ctx, Event{Category: Operation, Action: "Deploy", ActorName: "old-role", Params: `{"password":"Sensitive123","target":"db"}`})
		if i == 68 {
			l.Record(ctx, Event{Category: Security, Action: "LoginRejected"})
		}
	}
	// No journal_source metadata, as on an installation predating the module.
	path := l.path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(t.TempDir(), "logs.db")
	dest, err := logstore.Open(ctx, destPath, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportArchive(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("original database modified: %v", err)
	}
	renamed := path + ".archive"
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	dest, err = logstore.Open(ctx, destPath, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := ImportArchive(ctx, renamed, dest); err != nil {
		t.Fatal(err)
	}
	rows, err := dest.Query(ctx, logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 137 {
		t.Fatalf("renaming/restarting duplicated history: %d %v", len(rows), err)
	}
	for _, row := range rows {
		if strings.Contains(string(row.Data.(json.RawMessage)), "Sensitive123") {
			t.Fatal("archive import bypassed redaction")
		}
	}
	security, err := dest.Query(ctx, logstore.Filter{Category: logstore.Security})
	if err != nil || len(security) != 1 {
		t.Fatalf("lost security history: %v %v", security, err)
	}
	if err := ImportArchive(ctx, filepath.Join(t.TempDir(), "missing.db"), dest); err == nil {
		t.Fatal("missing source treated as empty")
	}
}

func TestArchiveImportReadsWALAndReusesExistingSourceIdentity(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	dest, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	l.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	if err := l.MigrateHistory(ctx, dest, 1); err != nil {
		t.Fatal(err)
	}
	// Leave the writer open so the second event is still in WAL. An immutable
	// main-file reader would silently miss this event after an unclean shutdown.
	l.Record(ctx, Event{Category: Operation, Action: "Stop"})
	if err := ImportArchive(ctx, l.path, dest); err != nil {
		t.Fatal(err)
	}
	rows, err := dest.Query(ctx, logstore.Filter{Category: logstore.Audit})
	if err != nil || len(rows) != 2 || rows[1].Kind != "panel.Stop" {
		t.Fatalf("WAL lost/source identity changed: %+v %v", rows, err)
	}
	if result, err := l.Verify(ctx); err != nil || !result.OK() || result.Records != 2 {
		t.Fatalf("original chain: %+v %v", result, err)
	}
}

func TestJournalRedactsLiveAndImportedAuditWithoutChangingOldChain(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	dest, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	inputs := []string{
		`{"nested":[{"password":"Sensitive123","command":"mysql -pSensitive456"}],"target":"safe-db"}`,
		`mysql -p Sensitive123 --token Sensitive456 safe-db`,
		`{"key":"Sensitive123", "target":"safe-db"`,
	}
	for i, params := range inputs {
		if i > 0 {
			l.Capture = func(e Event) { dest.Report(JournalEvent(e)) }
		}
		id := l.Record(ctx, Event{Category: Operation, Action: "Deploy", Params: params, Error: "failed mysql --password Sensitive789 postgres://u:SensitiveABC@safe-db/app"})
		old, err := l.Get(ctx, id)
		if err != nil || old.Params != params || !strings.Contains(old.Error, "Sensitive789") {
			t.Fatalf("old evidence changed: %+v %v", old, err)
		}
	}
	before, err := l.Get(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(ctx, dest, 3); err != nil {
		t.Fatal(err)
	}
	rows, err := dest.Query(ctx, logstore.Filter{Category: logstore.Audit})
	if err != nil || len(rows) != 3 {
		t.Fatalf("live/import reconciliation: %+v %v", rows, err)
	}
	for _, row := range rows {
		var copy Event
		if err := json.Unmarshal(row.Data.(json.RawMessage), &copy); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(copy.Params+copy.Error, "Sensitive") || !strings.Contains(copy.Params, "safe-db") || !strings.Contains(copy.Error, "safe-db") {
			t.Fatalf("unsafe or unusable audit copy: %+v", copy)
		}
	}
	after, err := l.Get(ctx, 3)
	if err != nil || before != after {
		t.Fatalf("original event/hash modified: %+v %+v %v", before, after, err)
	}
	if result, err := l.Verify(ctx); err != nil || !result.OK() || result.Records != 3 {
		t.Fatalf("old chain invalid: %+v %v", result, err)
	}
}

func TestPanelHistoryReconcilesLiveCaptureAndPreservesEvidence(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	dest, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "history.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	r := testRecordings(t)
	rec := r.Start(ctx, 80, 24)
	rec.Output([]byte("original encrypted recording"))
	summary := rec.Close()
	start := time.Date(2026, 11, 20, 1, 2, 3, 123456789, time.UTC)
	for i := range 137 {
		if i == 126 {
			l.Capture = func(e Event) { dest.Report(JournalEvent(e)) }
		}
		if l.Record(ctx, Event{Time: start.Add(time.Duration(i) * time.Second), Category: Session,
			Action: "OpenNodeShell", ProjectID: "project-a", TargetID: "node-a", RecordingID: summary.ID}) == 0 {
			t.Fatal("seed")
		}
	}
	if err := l.MigrateHistory(ctx, dest, 137); err != nil {
		t.Fatal(err)
	}
	assertHistory := func(want int) {
		t.Helper()
		rows, err := dest.Query(ctx, logstore.Filter{Category: logstore.Audit, Limit: 1000})
		if err != nil || len(rows) != want {
			t.Fatalf("history=%d want=%d err=%v", len(rows), want, err)
		}
		seen := map[int64]bool{}
		for _, row := range rows {
			var e Event
			if err := json.Unmarshal(row.Data.(json.RawMessage), &e); err != nil {
				t.Fatal(err)
			}
			if e.ActorName == "new database" {
				continue
			}
			if seen[e.ID] || e.RecordingID != summary.ID || row.Project != "project-a" || row.Object != "node-a" {
				t.Fatalf("duplicate or lost evidence: %+v", e)
			}
			seen[e.ID] = true
			wantTime := start.Add(time.Duration(e.ID-1) * time.Second)
			if row.Time.UnixMilli() != wantTime.UnixMilli() {
				t.Fatalf("wrong source time: %v want=%v", row.Time, wantTime)
			}
		}
	}
	assertHistory(137)
	if result, err := l.Verify(ctx); err != nil || !result.OK() || result.Records != 137 {
		t.Fatalf("migration changed verifiable chain: %+v %v", result, err)
	}
	if !strings.Contains(readAll(t, r, summary.ID), "original encrypted recording") {
		t.Fatal("migration lost recording")
	}
	// Renaming an old role's database must preserve its durable import identity;
	// a fresh role's database with IDs starting at one must not inherit it.
	oldPath := l.path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	archived := oldPath + ".old"
	if err := os.Rename(oldPath, archived); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, archived, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.MigrateHistory(ctx, dest, 137); err != nil {
		t.Fatal(err)
	}
	assertHistory(137)
	fresh, err := Open(ctx, oldPath, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.Record(ctx, Event{Time: start, Category: Operation, Action: "Deploy", ActorName: "new database"}) != 1 {
		t.Fatal("fresh database")
	}
	if err := fresh.MigrateHistory(ctx, dest, 1); err != nil {
		t.Fatal(err)
	}
	assertHistory(138)
}

func TestPanelHistoryResumesAfterSourceErrorAndReportsGaps(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	destPath := filepath.Join(t.TempDir(), "history.db")
	dest, err := logstore.Open(ctx, destPath, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { dest.Close() }()
	start := time.Now().Truncate(time.Millisecond)
	for i := range 137 {
		l.Record(ctx, Event{Time: start.Add(time.Duration(i) * time.Second), Category: Operation, Action: "Deploy"})
	}
	if _, err := l.DB().Exec(`UPDATE events SET time='corrupt' WHERE id=70`); err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(ctx, dest, 137); err == nil {
		t.Fatal("corrupt source silently accepted")
	}
	rows, err := dest.Query(ctx, logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 64 {
		t.Fatalf("completed batch was lost or corrupt batch advanced: %d %v", len(rows), err)
	}
	if _, err := l.DB().Exec(`UPDATE events SET time=? WHERE id=70`, start.Add(69*time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	dest, err = logstore.Open(ctx, destPath, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(ctx, dest, 137); err != nil {
		t.Fatal(err)
	}
	rows, err = dest.Query(ctx, logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 137 {
		t.Fatalf("restart lost/duplicated historical facts: %d %v", len(rows), err)
	}
	if result, err := l.Verify(ctx); err != nil || !result.OK() {
		t.Fatalf("repair and import changed chain: %+v %v", result, err)
	}
	// A new reader must report absent legacy IDs, not promise complete history.
	if err := l.deleteWhere(ctx, `id=2`); err != nil {
		t.Fatal(err)
	}
	other, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "other.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	head, err := l.HistoryHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(ctx, other, head); err != nil {
		t.Fatal(err)
	}
	gaps, err := other.Query(ctx, logstore.Filter{Category: logstore.System})
	if err != nil || len(gaps) != 1 || gaps[0].Kind != "legacy.panel.audit.gap" {
		t.Fatalf("missing legacy IDs hidden: %+v %v", gaps, err)
	}
	var gap struct{ From, To int64 }
	if err := json.Unmarshal(gaps[0].Data.(json.RawMessage), &gap); err != nil || gap.From != 2 || gap.To != 2 {
		t.Fatalf("wrong gap: %+v %v", gap, err)
	}
	if result, err := l.Verify(ctx); err != nil || !result.OK() {
		t.Fatalf("import changed tombstones: %+v %v", result, err)
	}
}

func TestUnimportedPanelAuditIsProtectedUnderSharedQuota(t *testing.T) {
	ctx := t.Context()
	l := openTestLog(t)
	for range 700 {
		if l.Record(ctx, Event{Category: Operation, Action: "Deploy", Params: strings.Repeat("x", MaxParams-20)}) == 0 {
			t.Fatal("seed")
		}
	}
	path := l.path
	l.Close()
	l, err := Open(ctx, path, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dest, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := l.UseBudget(ctx, dest, false); err != nil {
		t.Fatal(err)
	}
	if err := dest.Record(ctx, logstore.Event{Category: logstore.Audit, Kind: "before-import"}); !errors.Is(err, logstore.ErrCapacity) {
		t.Fatalf("quota deleted unimported source before worker started: %v", err)
	}
	if err := l.MigrateHistory(ctx, dest, 700); !errors.Is(err, logstore.ErrCapacity) {
		t.Fatalf("oversized protected source should require space: %v", err)
	}
	if result, err := l.Verify(ctx); err != nil || !result.OK() || result.Records != 700 {
		t.Fatalf("quota deleted original history: %+v %v", result, err)
	}
}
