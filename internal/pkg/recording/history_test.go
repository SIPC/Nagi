package recording

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestLegacyRecordingMigrationResumesWithoutChangingEvidence(t *testing.T) {
	r := testRecordings(t)
	good := r.WriteOnce(t.Context(), "independent-valid-recording")
	rec := r.Start(t.Context(), 89, 29)
	rec.Output(bytes.Repeat([]byte("first-block\r\n"), 30000))
	rec.Output([]byte("final-block-different-from-first"))
	broken := rec.Close()
	want := readAll(t, r, broken.ID)
	last := broken.Chunks[len(broken.Chunks)-1]
	if last.Seq == 0 {
		t.Fatal("fixture must have multiple independently committed chunks")
	}
	path := filepath.Join(r.Dir, broken.ID[:6], broken.ID, ChunkName(last.Seq))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt-ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "recordings.db")
	if err := r.UseDatabase(t.Context(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ImportLegacy(t.Context()); err == nil {
		t.Fatal("corrupt old body was acknowledged as migrated")
	}
	if !strings.Contains(readAll(t, r, good.ID), "independent-valid-recording") {
		t.Fatal("one bad source prevented independent history")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := &Recordings{Dir: r.Dir, Sealer: r.Sealer}
	if err := reopened.UseDatabase(t.Context(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ImportLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	var saved []byte
	if err := reopened.db.QueryRow(`SELECT body FROM recording_chunks WHERE recording=? AND seq=?`, broken.ID, last.Seq).Scan(&saved); err != nil || !bytes.Equal(saved, body) {
		t.Fatalf("changed original ciphertext/hash evidence: %v", err)
	}
	if original, err := os.ReadFile(path); err != nil || !bytes.Equal(original, body) {
		t.Fatal("migration modified or removed source")
	}
	if err := os.RemoveAll(filepath.Join(r.Dir, broken.ID[:6], broken.ID)); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, reopened, broken.ID); got != want {
		t.Fatal("resumed database playback differs from original")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reopened.ImportLegacy(ctx); err == nil {
		t.Fatal("canceled import ignored cancellation")
	}
}

func TestLegacyRecordingMigrationDoesNotResurrectPrunedBodies(t *testing.T) {
	r := testRecordings(t)
	summary := r.WriteOnce(t.Context(), "old-evidence")
	dbPath := filepath.Join(t.TempDir(), "recordings.db")
	if err := r.UseDatabase(t.Context(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ImportLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Model quota reclamation with an intact source still on disk. Import
	// receipts, unlike body rows, are not reclaimable history.
	if _, err := r.db.Exec(`DELETE FROM recording_sessions WHERE id=?`, summary.ID); err != nil {
		t.Fatal(err)
	}
	r.Close()
	reopened := &Recordings{Dir: r.Dir, Sealer: r.Sealer}
	if err := reopened.UseDatabase(t.Context(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ImportLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Read(summary.ID, func([]byte) error { return nil }); err == nil {
		t.Fatal("pruned body resurrected from legacy files after restart")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, summary.ID[:6], summary.ID, ChunkName(0))); err != nil {
		t.Fatal("test lost its still-present legacy source")
	}
}

func TestLegacyRecordingQuotaProtectsUnverifiedSource(t *testing.T) {
	r := testRecordings(t)
	summary := r.WriteOnce(t.Context(), "unverified-source")
	path := filepath.Join(r.Dir, summary.ID[:6], summary.ID, ChunkName(0))
	if err := os.WriteFile(path, []byte("bad-authentication-tag"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := r.UseDatabase(t.Context(), filepath.Join(t.TempDir(), "recordings.db"), journal); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.UseBudget(t.Context(), journal); err != nil {
		t.Fatal(err)
	}
	if err := r.ImportLegacy(t.Context()); err == nil {
		t.Fatal("invalid source accepted")
	}
	noise := make([]byte, 12000)
	rand.Read(noise)
	for i := 0; i < 100; i++ {
		if err := journal.Record(t.Context(), logstore.Event{Category: logstore.Audit, Kind: "fill", Data: map[string]any{"output": hex.EncodeToString(noise)}}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := journal.State(t.Context())
	if err != nil || len(state.Pruned) == 0 || state.DiskBytes > 2<<20 {
		t.Fatalf("quota pressure did not reclaim history within budget: %+v %v", state, err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "bad-authentication-tag" {
		t.Fatalf("quota deleted unverified original: %q %v", body, err)
	}
}
