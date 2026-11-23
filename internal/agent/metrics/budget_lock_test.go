package metrics

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// A node with many series (裸金属: 56 CPUs, 10 containers) at its cleanup line:
// background cleanup trims chart history while charts are being read. A long
// chart read must not make cleanup hold the journal lock waiting for the
// history database, which timed out ordinary log, task and alert writes.
func TestJournalWritesStayFastWhileChartReadAndCleanupOverlap(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	legacy, err := db.OpenMigrated(ctx, filepath.Join(dir, "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	path := filepath.Join(dir, "metrics.db")
	history, err := OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	start := time.Now().Add(-6 * time.Hour).Truncate(Interval).Unix()
	if _, err := history.ExecContext(ctx, `WITH RECURSIVE s(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM s WHERE i<199),
		t(j) AS (VALUES(0) UNION ALL SELECT j+1 FROM t WHERE j<1999)
		INSERT INTO metrics_raw SELECT printf('c:p/c%03d:net_rx',i), ?+j*10, i FROM s CROSS JOIN t`, start); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	size, err := logstore.FileBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	// History is two thirds of the write limit (budget*4/5); task records fill it
	// past the cleanup line. They rank above history, so cleanup trims history first.
	budget := size * 3 / 2 * 5 / 4
	journal, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), budget)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for {
		st, err := journal.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.DiskBytes+size > budget*4/5*93/100 {
			break
		}
		if err := journal.Record(ctx, logstore.Event{Category: logstore.Task, Kind: "task.output", Data: strings.Repeat("x", 32<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	c := New(history, nil, "", nil)
	defer c.CloseReader()
	if err := c.UseBudget(ctx, journal, path, true); err != nil {
		t.Fatal(err)
	}
	// A long chart read in progress, as when the panel pages through hours of
	// many series: the read keeps its snapshot open while rows stream out.
	stop := make(chan struct{})
	reading := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		rows, err := c.read().QueryContext(context.Background(), `SELECT series,ts,value FROM metrics_raw ORDER BY series,ts`)
		close(reading)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-reading
	for i := range 6 {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		began := time.Now()
		err := journal.Record(wctx, logstore.Event{Category: logstore.Task, Kind: "write", Data: fmt.Sprint(i)})
		cancel()
		if err != nil || time.Since(began) > 2*time.Second {
			close(stop)
			t.Fatalf("write %d during chart read and cleanup took %v: %v", i, time.Since(began), err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// The read finishes; cleanup then brings usage back under the cleanup line.
	close(stop)
	<-readDone
	limit := budget * 4 / 5
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := journal.Record(ctx, logstore.Event{Category: logstore.Task, Kind: "after"}); err != nil {
			t.Fatal(err)
		}
		st, err := journal.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.DiskBytes < limit*85/100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup stalled at %.1f%% of the limit after the chart read", 100*float64(st.DiskBytes)/float64(limit))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
