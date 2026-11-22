package logging

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// flush waits for queued Report batches; Report itself never waits for disk.
func flush(t *testing.T, logs *logstore.Store) {
	t.Helper()
	if err := logs.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// The alert engine reads the latest sample every round. With a long metrics
// history the read must start at the recent window, not walk the whole
// category under the shared log gate.
func TestLatestFreshDoesNotWalkMetricsHistory(t *testing.T) {
	now := time.Now().UTC()
	values := map[string]float64{"node:cpu": 3}
	for i := range 40 {
		values[fmt.Sprintf("c:p/service%d:mem", i)] = float64(i)
	}
	timed := func(history int) time.Duration {
		logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 256<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer logs.Close()
		collect := CaptureMetrics(logs, logstore.DefaultConfig())
		for i := history; i > 0; i-- {
			collect(now.Add(-time.Minute-time.Duration(i)*10*time.Second), values)
			if i%500 == 0 {
				flush(t, logs) // stay inside the bounded write queue
			}
		}
		collect(now.Add(-10*time.Second), values)
		flush(t, logs)
		s := Statistics{Store: logs}
		best := time.Hour
		for range 5 {
			t0 := time.Now()
			if got := s.LatestFresh(now); got["node:cpu"] != 3 || len(got) != 41 {
				t.Fatalf("latest sample incomplete: %d series", len(got))
			}
			best = min(best, time.Since(t0))
		}
		return best
	}
	short, long := timed(0), timed(20000)
	if long > 10*short+10*time.Millisecond {
		t.Fatalf("LatestFresh took %v after a long history, %v without", long, short)
	}
}

func TestRecordedStatisticsRequireCompleteFreshSamples(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	now := time.Now().UTC()
	collect := CaptureMetrics(logs, logstore.DefaultConfig())
	values := map[string]float64{"node:disk": 96, "node:cpu": 17,
		// 按挂载点、网卡、映射的序列名带 @ 和 /，不能被日志的脱敏当成 user:pass@ 或路径改写，否则磁盘、inode 告警看不到。
		"node:disk@/srv/nagi": 92, "node:filesystem@/srv/nagi:inodes_free": 7, "node:interface@eth0:rx_errors_per_second": 3}
	for i := range 64 {
		values[fmt.Sprintf("c:p/service%d:mem", i)] = float64(i + 9)
	}
	collect(now.Add(-20*time.Second), values)
	flush(t, logs)
	s := Statistics{Store: logs}
	got := s.LatestFresh(now)
	if len(got) != 69 || got["node:disk"] != 96 || got["c:p/service63:mem"] != 72 || got["node:disk@/srv/nagi"] != 92 ||
		got["node:filesystem@/srv/nagi:inodes_free"] != 7 || got["node:interface@eth0:rx_errors_per_second"] != 3 {
		t.Fatalf("chunked sample incomplete: %+v", got)
	}
	collect(now.Add(-10*time.Second), map[string]float64{"node:cpu": 19})
	flush(t, logs)
	got = s.LatestFresh(now)
	if len(got) != 1 || got["node:cpu"] != 19 {
		t.Fatalf("missing disk carried forward from old sample: %+v", got)
	}
	collect(now, map[string]float64{})
	flush(t, logs)
	if got := s.LatestFresh(now); len(got) != 0 {
		t.Fatalf("empty latest sample revived old healthy data: %+v", got)
	}
	if got := s.LatestFresh(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("stale sample accepted: %+v", got)
	}
	// Simulate a torn healthy batch through the public storage interface.
	if err := logs.Record(t.Context(), logstore.Event{Time: now.Add(time.Second), Category: logstore.Metrics, Kind: "sample", Data: map[string]any{"interval_seconds": 10, "values": []map[string]any{{"series": "node:disk", "value": 5}}}}); err != nil {
		t.Fatal(err)
	}
	if got := s.LatestFresh(now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("partly persisted batch accepted as healthy: %+v", got)
	}
	if _, err := s.Query(t.Context(), "r:", now.Add(-time.Minute), now); err == nil {
		t.Fatal("missing indexed history treated as empty healthy window")
	}
}

func TestRecordedWindowStatisticsUseIndexedLocalHistory(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE metrics_raw(series TEXT,ts INTEGER,value REAL,PRIMARY KEY(series,ts)); CREATE TABLE metrics_m5(series TEXT,ts INTEGER,value REAL,PRIMARY KEY(series,ts));`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	for i, v := range []float64{.25, 7, 19} {
		if _, err := d.Exec(`INSERT INTO metrics_raw VALUES ('r:p/web:req',?,?)`, at.Add(time.Duration(i)*10*time.Second).Unix(), v); err != nil {
			t.Fatal(err)
		}
	}
	s := Statistics{History: metrics.New(d, nil, "", nil)}
	got, err := s.Query(ctx, "r:p/web:", at.Add(10*time.Second), at.Add(20*time.Second))
	if err != nil || len(got["r:p/web:req"]) != 2 || got["r:p/web:req"][0].Value != 7 || got["r:p/web:req"][1].Value != 19 {
		b, _ := json.Marshal(got)
		t.Fatalf("window boundaries/values changed: %s %v", b, err)
	}
}
