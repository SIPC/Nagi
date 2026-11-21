package server

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/server/audit"
)

// An upgraded panel finds its audit chain in the panel directory. On start the
// chain moves to the local log directory, verifies there on its own, continues
// with the old last hash, and nothing is left beside the business database.
func TestPanelAuditLeavesPanelDirectory(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	panelDir := filepath.Join(root, "panel")
	old, err := audit.Open(ctx, filepath.Join(panelDir, "audit.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for range 25 {
		old.Record(ctx, audit.Event{Category: audit.Operation, Action: "Deploy"})
	}
	before, err := old.Verify(ctx)
	if err != nil || !before.OK() {
		t.Fatal(before, err)
	}
	old.Close()
	logs, err := logstore.Open(ctx, filepath.Join(root, "logs", "logs.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()

	s, err := New(ctx, Config{DataDir: panelDir, Logger: slog.New(slog.DiscardHandler), Logs: logs, LogConfig: logstore.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.listener.Close()
		if s.mesh != nil {
			s.mesh.Close()
		}
		s.recordings.Close()
		s.audit.Close()
		s.db.Close()
	}()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(panelDir, "audit.db"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy audit.db%s still beside panel.db", suffix)
		}
	}
	id := s.audit.Record(ctx, audit.Event{Category: audit.Operation, Action: "AfterUpgrade"})
	e, err := s.audit.Get(ctx, id)
	if err != nil || e.PrevHash != before.LastHash || id != before.LastID+1 {
		t.Fatalf("chain does not continue: %+v %v", e, err)
	}
	r, err := audit.VerifyFile(ctx, filepath.Join(root, "logs", "audit", "audit.db"))
	if err != nil || !r.OK() || r.Records != before.Records+1 {
		t.Fatalf("relocated chain: %+v %v", r, err)
	}
	s.verifyMovedAudit(ctx) // runs in the background once the panel serves
	for _, m := range logs.Migrations() {
		if m.ID == audit.RelocateID && m.Phase != "complete" {
			t.Fatalf("migration status: %+v", m)
		}
	}
}
