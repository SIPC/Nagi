package metrics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestSeparateHistoryPreservesQueriesAndNodeState(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	legacy, err := db.OpenMigrated(ctx, filepath.Join(dir, "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`INSERT INTO subnets VALUES ('10.5.0.0/24','p:keep'); INSERT INTO containers(project_id,name,dir,spec,created_at) VALUES ('keep','web','/srv/keep',X'010203',1)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(Interval)
	for i := range 517 {
		if _, err := legacy.Exec(`INSERT INTO metrics_raw VALUES (?,?,?)`, fmt.Sprintf("c:p/s-%03d:cpu", i), at.Unix(), float64(i)+0.25); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(`INSERT INTO metrics_m5 VALUES (?,?,?)`, fmt.Sprintf("c:p/s-%03d:cpu", i), at.Add(-48*time.Hour).Unix(), float64(i)+0.75); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "logs", "metrics.db")
	history, err := OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	for _, table := range []string{"metrics_raw", "metrics_m5"} {
		var n int
		if err := legacy.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("旧历史仍在写入库: %s %d %v", table, n, err)
		}
	}
	c := New(history, nil, "", nil)
	for _, tc := range []struct {
		at    time.Time
		value float64
	}{{at, 516.25}, {at.Add(-48 * time.Hour), 516.75}} {
		data, err := c.Query(ctx, "c:p/s-516:", tc.at.Add(-time.Second), tc.at.Add(time.Second))
		if err != nil || len(data["c:p/s-516:cpu"]) != 1 || data["c:p/s-516:cpu"][0].Value != tc.value {
			t.Fatalf("分离后图表历史改变: %+v %v", data, err)
		}
	}
	if err := c.store(ctx, at.Add(Interval), map[string]float64{"node:cpu": 71}); err != nil {
		t.Fatal(err)
	}
	history.Close()
	history, err = OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	data, err := New(history, nil, "", nil).Query(ctx, "node:", at, at.Add(2*Interval))
	if err != nil || len(data["node:cpu"]) != 1 || data["node:cpu"][0].Value != 71 {
		t.Fatalf("重启丢失新采样: %+v %v", data, err)
	}
	var subnet, spec string
	if err := legacy.QueryRow(`SELECT subnet FROM subnets WHERE owner='p:keep'`).Scan(&subnet); err != nil || subnet != "10.5.0.0/24" {
		t.Fatalf("网段改变: %q %v", subnet, err)
	}
	if err := legacy.QueryRow(`SELECT hex(spec) FROM containers WHERE project_id='keep'`).Scan(&spec); err != nil || spec != "010203" {
		t.Fatalf("配置改变: %q %v", spec, err)
	}
}

func TestHistoryConflictAndCancellationKeepBothSourceTables(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	legacy, err := db.OpenMigrated(ctx, filepath.Join(dir, "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	_, err = legacy.Exec(`INSERT INTO metrics_raw VALUES ('node:cpu',10,12); INSERT INTO metrics_m5 VALUES ('node:cpu',0,34)`)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metrics.db")
	target, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Exec(`CREATE TABLE metrics_m5(series TEXT NOT NULL,ts INTEGER NOT NULL,value REAL NOT NULL,PRIMARY KEY(series,ts)) WITHOUT ROWID; INSERT INTO metrics_m5 VALUES ('node:cpu',0,99)`)
	target.Close()
	if err != nil {
		t.Fatal(err)
	}
	if d, err := OpenHistory(ctx, path, legacy, nil); err == nil {
		d.Close()
		t.Fatal("冲突被静默覆盖")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if d, err := OpenHistory(cancelled, path, legacy, nil); err == nil {
		d.Close()
		t.Fatal("取消后继续迁移")
	}
	for _, tc := range []struct {
		table string
		value float64
	}{{"metrics_raw", 12}, {"metrics_m5", 34}} {
		var n int
		var value float64
		if err := legacy.QueryRow(`SELECT count(*),value FROM `+tc.table).Scan(&n, &value); err != nil || n != 1 || value != tc.value {
			t.Fatalf("失败后源历史受损: %s %d %v %v", tc.table, n, value, err)
		}
	}
	target, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Exec(`UPDATE metrics_m5 SET value=34`)
	target.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON metrics_m5 BEGIN SELECT RAISE(ABORT,'test delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenHistory(ctx, path, legacy, nil); err == nil {
		d.Close()
		t.Fatal("源删除失败没有报错")
	}
	var left int
	if err := legacy.QueryRow(`SELECT count(*) FROM metrics_raw`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("第二张源表删除失败时没有回滚第一张: %d %v", left, err)
	}
	if _, err := legacy.Exec(`DROP TRIGGER reject_delete`); err != nil {
		t.Fatal(err)
	}
	target, err = OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.QueryRow(`SELECT (SELECT count(*) FROM metrics_raw)+(SELECT count(*) FROM metrics_m5)`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("中断后的重试丢失或重复: %d %v", left, err)
	}
}

func TestMigrationCopiesAndFallbackWritesShareCapacity(t *testing.T) {
	for _, capacity := range []int64{4 << 20, 32 << 20} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			ctx, dir := t.Context(), t.TempDir()
			legacyPath := filepath.Join(dir, "node.db")
			legacy, err := db.OpenMigrated(ctx, legacyPath, nodestore.Migrations)
			if err != nil {
				t.Fatal(err)
			}
			defer legacy.Close()
			if _, err := legacy.Exec(`INSERT INTO subnets VALUES ('10.5.0.0/24','keep')`); err != nil {
				t.Fatal(err)
			}
			tx, err := legacy.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2100 {
				if _, err := tx.Exec(`INSERT INTO metrics_raw VALUES (?,?,?)`, "c:p/"+strings.Repeat("s", 512)+fmt.Sprint(i), 10, float64(i)); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(`INSERT INTO metrics_m5 VALUES ('node:cpu',0,34)`); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			journal, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), capacity)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			path := filepath.Join(dir, "metrics.db")
			history, migrateErr := OpenHistory(ctx, path, legacy, journal)
			if capacity == 4<<20 {
				if !errors.Is(migrateErr, logstore.ErrCapacity) {
					if history != nil {
						history.Close()
					}
					t.Fatalf("expected bounded migration failure: %v", migrateErr)
				}
				for table, want := range map[string]int{"metrics_raw": 2100, "metrics_m5": 1} {
					var n int
					if err := legacy.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != want {
						t.Fatalf("protected source changed: %s %d %v", table, n, err)
					}
				}
				c := New(legacy, nil, "", nil)
				if err := c.UseBudget(ctx, journal, path, false); err != nil {
					t.Fatal(err)
				}
				values := map[string]float64{}
				for i := range 64 {
					values[fmt.Sprint(i)] = float64(i)
				}
				if err := c.store(ctx, time.Now(), values); !errors.Is(err, logstore.ErrCapacity) {
					t.Fatalf("fallback bypassed quota: %v", err)
				}
			} else {
				if migrateErr != nil {
					t.Fatal(migrateErr)
				}
				defer history.Close()
				c := New(history, nil, "", nil)
				if err := c.UseBudget(ctx, journal, path, true); err != nil {
					t.Fatal(err)
				}
				var n int
				if err := history.QueryRow(`SELECT count(*) FROM metrics_raw`).Scan(&n); err != nil || n != 2100 {
					t.Fatalf("migration changed history: %d %v", n, err)
				}
			}
			var subnet string
			if err := legacy.QueryRow(`SELECT subnet FROM subnets WHERE owner='keep'`).Scan(&subnet); err != nil || subnet != "10.5.0.0/24" {
				t.Fatalf("subnet changed: %q %v", subnet, err)
			}
			state, err := journal.State(ctx)
			oldBytes, _ := logstore.FileBytes(legacyPath)
			newBytes, _ := logstore.FileBytes(path)
			want := newBytes
			if migrateErr != nil {
				want += oldBytes
			}
			if err != nil || state.DiskBytes > capacity || len(state.Archives) != 1 || state.Archives[0].DiskBytes != want {
				t.Fatalf("migration files not accounted: %+v want %d err %v", state, want, err)
			}
			if migrateErr != nil {
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), 32<<20)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				resumed, err := OpenHistory(ctx, path, legacy, reopened)
				if err != nil {
					t.Fatal(err)
				}
				defer resumed.Close()
				var n int
				var sum float64
				if err := resumed.QueryRow(`SELECT count(*),sum(value) FROM metrics_raw`).Scan(&n, &sum); err != nil || n != 2100 || sum != 2203950 {
					t.Fatalf("resumed migration changed samples: %d %v %v", n, sum, err)
				}
			}
		})
	}
}
