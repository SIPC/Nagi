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

func TestRecordedStatisticsRequireCompleteFreshSamples(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	now := time.Now().UTC()
	collect := CaptureMetrics(logs, logstore.DefaultConfig())
	values := map[string]float64{"node:disk": 96, "node:cpu": 17}
	for i := range 67 {
		values[fmt.Sprintf("c:p/service%d:mem", i)] = float64(i + 9)
	}
	collect(now.Add(-20*time.Second), values)
	s := Statistics{Store: logs}
	got := s.LatestFresh(now)
	if len(got) != 69 || got["node:disk"] != 96 || got["c:p/service66:mem"] != 75 {
		t.Fatalf("chunked sample incomplete: %+v", got)
	}
	collect(now.Add(-10*time.Second), map[string]float64{"node:cpu": 19})
	got = s.LatestFresh(now)
	if len(got) != 1 || got["node:cpu"] != 19 {
		t.Fatalf("missing disk carried forward from old sample: %+v", got)
	}
	collect(now, map[string]float64{})
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
