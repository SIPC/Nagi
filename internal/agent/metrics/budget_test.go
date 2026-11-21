package metrics

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestMetricHistorySharesPhysicalBudgetAndRetainsAudit(t *testing.T) {
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
	now := time.Now().Truncate(Interval)
	tx, err := history.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2100 {
		if _, err := tx.Exec(`INSERT INTO metrics_raw VALUES (?,?,?)`, "c:p/"+strings.Repeat("s", 512)+fmt.Sprint(i)+":cpu", now.Add(-time.Hour).Unix(), float64(i)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO metrics_m5 VALUES ('node:cpu',?,19)`, now.Add(-60*24*time.Hour).Unix()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	journal, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := New(history, nil, "", nil)
	if err := c.UseBudget(ctx, journal, path, true); err != nil {
		t.Fatal(err)
	}
	c.lastDownsampledTo = now.Truncate(bucket)
	c.maintain(ctx, now)
	old, err := c.Query(ctx, "node:", now.Add(-61*24*time.Hour), now.Add(-59*24*time.Hour))
	if err != nil || len(old["node:cpu"]) != 1 || old["node:cpu"][0].Value != 19 {
		t.Fatalf("预算接管后仍按天数删除历史: %+v %v", old, err)
	}
	for i := range 30 {
		if err := journal.Record(ctx, logstore.Event{Category: logstore.Audit, Kind: "operation", Data: map[string]any{"message": strings.Repeat("a", 48<<10)}}); err != nil {
			t.Fatalf("审计 %d: %v", i, err)
		}
		st, err := journal.State(ctx)
		if err != nil || st.DiskBytes > 4<<20 {
			t.Fatalf("新旧物理占盘没有合计: %+v %v", st, err)
		}
	}
	if err := c.store(ctx, now, map[string]float64{"node:cpu": 77}); err != nil {
		t.Fatal(err)
	}
	data, err := c.Query(ctx, "node:", now.Add(-time.Second), now.Add(time.Second))
	if err != nil || len(data["node:cpu"]) != 1 || data["node:cpu"][0].Value != 77 {
		t.Fatalf("容量清理破坏当前读取: %+v %v", data, err)
	}
	audits, err := journal.Query(ctx, logstore.Filter{Category: logstore.Audit, Limit: 1000})
	if err != nil || len(audits) != 30 {
		t.Fatalf("普通指标挤掉审计: %d %v", len(audits), err)
	}
	st, err := journal.State(ctx)
	if err != nil || len(st.Archives) != 1 || st.Archives[0].PrunedBatches == 0 || st.Archives[0].DiskBytes == 0 {
		t.Fatalf("旧库清理与真实占盘不可见: %+v %v", st, err)
	}
}
