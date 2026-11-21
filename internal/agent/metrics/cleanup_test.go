package metrics

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// 旧监控表只在迁移核验、清空之后才删；表里还有数据时拒绝，原样保留。
// 删表后、写标记前被杀，重启接着删、回收空间；全部完成后再启动不重复动作，迁移也照常。
func TestCleanupLegacyOnlyAfterVerifiedMigration(t *testing.T) {
	ctx, dir := t.Context(), t.TempDir()
	nodePath := filepath.Join(dir, "node.db")
	legacy, err := db.OpenMigrated(ctx, nodePath, nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`INSERT INTO subnets VALUES ('10.5.0.0/24','p:keep');
		WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<20000)
		INSERT INTO metrics_raw SELECT 'node:cpu-'||(x%50),1700000000+x,x+0.5 FROM n`); err != nil {
		t.Fatal(err)
	}
	logs, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tables := func() int {
		var n int
		if err := legacy.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('metrics_raw','metrics_m5')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	marker := func() string {
		b, err := logs.Checkpoint(ctx, cleanupCheckpoint)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// 未迁移（表里还有数据）：不删，不写标记，在日志页提示失败，表还在。
	if err := CleanupLegacy(ctx, legacy, nodePath, logs, log); err == nil || tables() != 2 || marker() != "" {
		t.Fatalf("未清空的旧表不能删：err=%v tables=%d marker=%s", err, tables(), marker())
	}
	if m := logs.Migrations(); len(m) != 1 || m[0].ID != CleanupID || m[0].Phase != "failed" {
		t.Fatalf("失败应在日志页提示：%+v", m)
	}

	// 迁移核验并清空后：删表、回收空间、写完成标记。
	path := filepath.Join(dir, "logs", "metrics.db")
	history, err := OpenHistory(ctx, path, legacy, logs)
	if err != nil {
		t.Fatal(err)
	}
	// 中途被杀：一张表已删、提交了，还没写标记，也没回收空间。
	if _, err := legacy.Exec(`DROP TABLE metrics_raw`); err != nil {
		t.Fatal(err)
	}
	before, _ := logstore.FileBytes(nodePath)
	if err := CleanupLegacy(ctx, legacy, nodePath, logs, log); err != nil {
		t.Fatal(err)
	}
	after, _ := logstore.FileBytes(nodePath)
	if tables() != 0 || marker() != `{"phase":"done"}` || after >= before {
		t.Fatalf("应删表、回收空间并标记完成：tables=%d marker=%s %d→%d", tables(), marker(), before, after)
	}
	var subnet string
	if err := legacy.QueryRow(`SELECT subnet FROM subnets WHERE owner='p:keep'`).Scan(&subnet); err != nil || subnet != "10.5.0.0/24" {
		t.Fatalf("业务数据改变：%q %v", subnet, err)
	}
	var n int
	var sum float64
	if err := history.QueryRow(`SELECT count(*), sum(value) FROM metrics_raw`).Scan(&n, &sum); err != nil || n != 20001 || sum != 20001*10000.5 {
		history.Close()
		t.Fatalf("新库应保有全部迁移数据：%d %v %v", n, sum, err)
	}
	history.Close()

	// 再次启动：旧表已不在，迁移照常打开新库；清理不重复。
	history, err = OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	history.Close()
	if err := CleanupLegacy(context.Background(), legacy, nodePath, logs, log); err != nil || marker() != `{"phase":"done"}` {
		t.Fatalf("完成后不应重复清理：%v %s", err, marker())
	}
}
