package agent

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Docker engine OOM events are Task records and kernel lines are Runtime
// records. Each event counts once, events from before the node started do not
// count, and lookalike kinds in other categories are not counted.
func TestLogEventAlertsCountOnceFromTheirCategories(t *testing.T) {
	ctx := t.Context()
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	write := func(e logstore.Event) {
		t.Helper()
		if err := logs.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	oom := func(container string) logstore.Event {
		return logstore.Event{Category: logstore.Task, Kind: "docker.container.oom", Project: "0123456789abcdef",
			Data: map[string]any{"attributes": map[string]string{"nagi.container": container}}}
	}
	write(oom("before-start"))
	// Unrelated history in other categories, as on a busy node.
	batch := make([]logstore.Event, 0, 2000)
	for i := range 20000 {
		batch = append(batch, logstore.Event{Category: logstore.Access, Kind: "request", Data: map[string]any{"n": i, "path": "/static/app.js"}})
		if len(batch) == cap(batch) {
			if err := logs.ImportBatch(ctx, batch, "test.history", i); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	var counted []string
	a := &agent{cfg: Config{Logs: logs}, countAlert: func(key, _, _, _, _ string, _ time.Time) { counted = append(counted, key) }}

	a.pollLogEvents(ctx, time.Now())
	if len(counted) != 0 {
		t.Fatalf("events written before the node started were counted: %v", counted)
	}
	// Before anything matches, an idle round must not rescan the history: the
	// poll runs every 30 seconds while holding the log store's gate that alert
	// and audit writes wait for. Compare with one scan for the same kinds.
	idle := time.Duration(1<<63 - 1)
	for range 3 {
		began := time.Now()
		a.pollLogEvents(ctx, time.Now())
		idle = min(idle, time.Since(began))
	}
	began := time.Now()
	if _, err := logs.Query(ctx, logstore.Filter{KindPrefix: "host.kernel.", Limit: 1000}); err != nil {
		t.Fatal(err)
	}
	scan := time.Since(began)
	if idle*5 >= scan {
		t.Fatalf("an idle poll took %v, a whole-store scan %v: the poll is rescanning history", idle, scan)
	}
	write(oom("web"))
	write(logstore.Event{Category: logstore.Runtime, Kind: "host.kernel.memory", Data: map[string]any{"message": "Out of memory: Killed process 42"}})
	write(logstore.Event{Category: logstore.Runtime, Kind: "host.kernel.storage", Data: map[string]any{"message": "EXT4-fs error"}})
	write(logstore.Event{Category: logstore.Container, Kind: "docker.container.oom", Data: "a container printed this kind"})
	a.pollLogEvents(ctx, time.Now())
	a.pollLogEvents(ctx, time.Now())
	slices.Sort(counted)
	want := []string{"container_oom:0123456789abcdef/web", "kernel_oom", "kernel_storage"}
	if !slices.Equal(counted, want) {
		t.Fatalf("counted %v, want %v", counted, want)
	}
}
