package recording

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestDatabaseRecordingRestartLegacyAndEncryption(t *testing.T) {
	r := testRecordings(t)
	legacy := r.WriteOnce(t.Context(), "older-file-output")
	path := filepath.Join(t.TempDir(), "recordings.db")
	if err := r.UseDatabase(t.Context(), path, nil); err != nil {
		t.Fatal(err)
	}
	rec := r.Start(t.Context(), 93, 31)
	rec.Output([]byte("new-database-output 密码"))
	rec.Resize(117, 42)
	summary := rec.Close()
	if summary.Truncated || len(summary.Chunks) != 1 || summary.Chunks[0].Hash == "" {
		t.Fatalf("recording summary: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, summary.ID[:6], summary.ID)); !os.IsNotExist(err) {
		t.Fatalf("new body must not also enter old file replication: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := &Recordings{Dir: r.Dir, Sealer: r.Sealer}
	if err := reopened.UseDatabase(t.Context(), path, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got := readAll(t, reopened, legacy.ID); !strings.Contains(got, "older-file-output") {
		t.Fatalf("legacy body lost: %q", got)
	}
	got := readAll(t, reopened, summary.ID)
	for _, want := range []string{`"width":93`, "new-database-output 密码", `"117x42"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %q", want, got)
		}
	}
	wrongKey := testRecordings(t)
	key := reopened.Sealer
	reopened.Sealer = wrongKey.Sealer
	if err := reopened.Read(summary.ID, func([]byte) error { return nil }); err == nil {
		t.Fatal("body decrypted with a different key")
	}
	reopened.Sealer = key
	// Corrupt persisted ciphertext, not the summary: replay must reject it.
	var body []byte
	if err := reopened.db.QueryRow(`SELECT body FROM recording_chunks WHERE recording=?`, summary.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	body[len(body)-1] ^= 0xff
	if _, err := reopened.db.Exec(`UPDATE recording_chunks SET body=? WHERE recording=?`, body, summary.ID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Read(summary.ID, func([]byte) error { return nil }); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
}

func TestDatabaseRecordingSharesQuotaAndProtectsActive(t *testing.T) {
	const capacity = 8 << 20
	journal, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	r := testRecordings(t)
	if err := r.UseDatabase(t.Context(), filepath.Join(t.TempDir(), "recordings.db"), journal); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	old := r.WriteOnce(t.Context(), "finished recording")
	active := r.Start(t.Context(), 80, 24)
	defer active.Close()
	active.Output(bytes.Repeat([]byte("still-open-output\r\n"), 20000))
	data := make([]byte, 12000)
	rand.Read(data)
	pruned := false
	for i := 0; i < 600; i++ {
		if err := journal.Record(t.Context(), logstore.Event{Category: logstore.Audit, Kind: "test.fill", Data: map[string]any{"output": hex.EncodeToString(data)}}); err != nil {
			t.Fatal(err)
		}
		if err := r.Read(old.ID, func([]byte) error { return nil }); err != nil {
			pruned = true
			break
		}
	}
	if !pruned {
		t.Fatal("journal writes did not reclaim old recording storage")
	}
	if got := readAll(t, r, active.ID()); !strings.Contains(got, "still-open-output") {
		t.Fatal("active recording was reclaimed")
	}
	// The budget accounts for the database, index, WAL and SHM, not just bodies.
	usage, err := journal.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.DiskBytes > capacity {
		t.Fatalf("shared quota exceeded: %+v", usage)
	}
}
