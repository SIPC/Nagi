package auditlog

import (
	"bytes"
	"context"
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

func TestLegacyAuditHistoryJoinsIndependentCaptureWithoutDuplicates(t *testing.T) {
	old := maxSize
	maxSize = 12 << 10
	t.Cleanup(func() { maxSize = old })
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dest, err := logstore.Open(t.Context(), filepath.Join(dir, "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := l.UseBudget(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 137; i++ {
		if i == 71 {
			l.Capture = func(e Event) {
				dest.Report(logstore.Event{Time: e.Time, Category: logstore.Audit, Kind: "node." + e.Action, Object: e.Target, Data: e})
			}
		}
		if seq := l.Record(Event{Time: time.Unix(1700000000+int64(i), 123), Action: "DeployContainer", Target: "p/web", Detail: "password=do-not-import"}); seq != int64(i) {
			t.Fatal(seq)
		}
	}
	before, _, _ := l.After(0, 1000)
	for range 2 {
		if err := l.MigrateHistory(t.Context(), dest, l.Seq()); err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ := l.After(0, 1000)
	if len(before) != 137 || !bytes.Equal(bytes.Join(before, nil), bytes.Join(after, nil)) {
		t.Fatal("migration modified the legacy source")
	}
	rows, err := dest.Query(t.Context(), logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 137 {
		t.Fatalf("lost or duplicated history: %d %v", len(rows), err)
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		var e Event
		if err := json.Unmarshal(row.Data.(json.RawMessage), &e); err != nil || seen[e.Seq] || e.Seq < 1 || e.Seq > 137 || row.Time.Unix() != 1700000000+e.Seq || row.Object != "p/web" {
			t.Fatalf("changed identity/time/object: %+v %v", row, err)
		}
		seen[e.Seq] = true
		if e.Detail != "[已隐藏]" {
			t.Fatal("migration bypassed redaction")
		}
	}
}

func TestLegacyAuditMigrationStopsAtCorruptionAndResumesAfterRepair(t *testing.T) {
	dir := t.TempDir()
	var lines [][]byte
	for i := 1; i <= 66; i++ {
		b, _ := json.Marshal(Event{Seq: int64(i), Time: time.Unix(1700000000+int64(i), 0), Action: "NodeExec", Result: "ok"})
		lines = append(lines, append(b, '\n'))
	}
	correct := append([]byte(nil), lines[64]...)
	lines[64] = []byte("{corrupt}\n")
	if err := os.WriteFile(filepath.Join(dir, current), bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := l.MigrateHistory(t.Context(), dest, 66); err == nil {
		t.Fatal("corrupt source was silently skipped")
	}
	rows, err := dest.Query(t.Context(), logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 64 {
		t.Fatalf("valid first batch was not durable: %d %v", len(rows), err)
	}
	l.Close()
	lines[64] = correct
	if err := os.WriteFile(filepath.Join(dir, current), bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.MigrateHistory(t.Context(), dest, l.Seq()); err != nil {
		t.Fatal(err)
	}
	rows, err = dest.Query(t.Context(), logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(rows) != 66 {
		t.Fatalf("restart did not resume without duplicates: %d %v", len(rows), err)
	}
}

func TestLegacyAuditMigrationCannotPruneItsUnimportedSource(t *testing.T) {
	old := maxSize
	maxSize = 1000
	t.Cleanup(func() { maxSize = old })
	l, err := Open(filepath.Join(t.TempDir(), "audit"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := range 8 {
		l.Record(Event{Action: fmt.Sprint(i), Detail: strings.Repeat("x", 600)})
	}
	before, _, _ := l.After(0, 100)
	dest, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := l.UseBudget(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(protected, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dest.RegisterArchive(t.Context(), logstore.Archive{Name: protected, Priority: 4,
		Size: func() (int64, error) {
			st, err := os.Stat(protected)
			if err != nil {
				return 0, err
			}
			return st.Size(), nil
		},
		Trim: func(context.Context) (bool, error) { return false, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(t.Context(), dest, l.Seq()); !errors.Is(err, logstore.ErrCapacity) {
		t.Fatalf("expected explicit capacity refusal, got %v", err)
	}
	after, _, _ := l.After(0, 100)
	if len(before) != 8 || !bytes.Equal(bytes.Join(before, nil), bytes.Join(after, nil)) {
		t.Fatal("migration deleted source history to make room for its copy")
	}
	if err := os.Remove(protected); err != nil {
		t.Fatal(err)
	}
	if err := dest.ReplaceArchive(t.Context(), logstore.Archive{Name: protected, Priority: 4,
		Size: func() (int64, error) { return 0, nil }, Trim: func(context.Context) (bool, error) { return false, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.MigrateHistory(t.Context(), dest, l.Seq()); err != nil {
		t.Fatal(err)
	}
	rows, err := dest.Query(t.Context(), logstore.Filter{Category: logstore.Audit, Limit: 100})
	if err != nil || len(rows) != 8 {
		t.Fatalf("capacity recovery did not resume: %d %v", len(rows), err)
	}
}

func TestLegacyAuditMigrationReportsExistingGaps(t *testing.T) {
	dir := t.TempDir()
	var lines [][]byte
	for _, seq := range []int64{3, 4, 8} {
		b, _ := json.Marshal(Event{Seq: seq, Time: time.Unix(1700000000+seq, 0), Action: "NodeExec"})
		lines = append(lines, append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, current), bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dest, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := l.MigrateHistory(t.Context(), dest, l.Seq()); err != nil {
		t.Fatal(err)
	}
	rows, err := dest.Query(t.Context(), logstore.Filter{Category: logstore.System})
	if err != nil {
		t.Fatal(err)
	}
	var gaps [][2]int64
	for _, row := range rows {
		if row.Kind == "legacy.audit.gap" {
			var gap struct{ From, To int64 }
			if err := json.Unmarshal(row.Data.(json.RawMessage), &gap); err != nil {
				t.Fatal(err)
			}
			gaps = append(gaps, [2]int64{gap.From, gap.To})
		}
	}
	if len(gaps) != 2 || gaps[0] != [2]int64{1, 2} || gaps[1] != [2]int64{5, 7} {
		t.Fatalf("missing source history was hidden or overstated: %v", gaps)
	}
}
