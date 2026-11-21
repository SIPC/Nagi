package logging

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Opt in only in an isolated system journal containing exactly fifteen fixture
// manager failures. Never run fixture assertions against a developer's host.
func TestHostSystemJournalIntegration(t *testing.T) {
	if os.Getenv("NAGI_TEST_HOST_JOURNAL") != "1" {
		t.Skip("requires isolated journal fixture")
	}
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		RunHost(ctx, logs, logstore.DefaultConfig())
		cancel()
		rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Runtime})
		if err != nil || len(rows) != 15 {
			status, _ := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
			for _, row := range status {
				t.Logf("%s: %s", row.Kind, row.Data)
			}
			t.Fatalf("full backlog/resume: %d rows %v", len(rows), err)
		}
		b, err := logs.Checkpoint(t.Context(), "host.journal")
		var cursor string
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor == "" {
			t.Fatalf("durable cursor: %s %v", b, err)
		}
		if err := (systemJournal{}).check(t.Context(), cursor); err != nil {
			t.Fatalf("cannot seek to committed cursor: %v", err)
		}
	}
}
