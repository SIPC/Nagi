package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// cleanupCheckpoint records how far removing the legacy metric tables from
// node.db has got. Each step is idempotent, so a kill at any point reruns safely.
const cleanupCheckpoint = "legacy.cleanup.metrics"

// CleanupID is the live migration entry that reports the cleanup to the log page.
const CleanupID = "cleanup.metrics"

type cleanupState struct {
	Phase string `json:"phase"` // "", dropped, done
}

// CleanupLegacy drops the legacy metric tables from node.db and reclaims the
// space. Call it only after MigrateHistory returned without error: that copied
// and compared every value before emptying both tables. The drop itself
// re-checks, in the same transaction, that both tables are still empty.
// On failure nothing more is removed; the next start retries.
func CleanupLegacy(ctx context.Context, legacy *sql.DB, path string, logs *logstore.Store, log *slog.Logger) (err error) {
	if logs == nil {
		return nil
	}
	report := func(phase, msg string) {
		logs.SetMigration(logstore.Migration{ID: CleanupID, Phase: phase, Error: msg})
	}
	defer func() {
		if err != nil && ctx.Err() == nil {
			report("failed", err.Error())
		}
	}()
	var st cleanupState
	if b, err := logs.Checkpoint(ctx, cleanupCheckpoint); err != nil {
		return err
	} else if len(b) != 0 {
		if err := json.Unmarshal(b, &st); err != nil {
			return err
		}
	}
	mark := func(phase string, data map[string]any) error {
		data["phase"], data["target"] = phase, "node.db metrics_raw, metrics_m5"
		st.Phase = phase
		return logs.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.cleanup.metrics." + phase, Data: data}, cleanupCheckpoint, st)
	}
	if st.Phase == "done" {
		return nil
	}
	report("cleanup", "")
	if st.Phase == "" {
		tx, err := legacy.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var tables, rows int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('metrics_raw','metrics_m5')`).Scan(&tables); err != nil {
			return err
		}
		if tables == 2 {
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM metrics_raw)+(SELECT count(*) FROM metrics_m5)`).Scan(&rows); err != nil {
				return err
			}
		}
		if rows != 0 {
			return fmt.Errorf("node.db 的旧监控表仍有 %d 行，未删除", rows)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS metrics_raw; DROP TABLE IF EXISTS metrics_m5`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Info("已删除 node.db 中迁移后清空的旧监控表")
		if err := mark("dropped", map[string]any{"verified": "迁移已逐值核对并清空旧表，删表时同一事务内再确认为空"}); err != nil {
			return err
		}
	}
	// VACUUM rewrites only the remaining configuration, so it is short; it still
	// holds the write lock, which other node.db writers wait out (busy_timeout).
	before, err := logstore.FileBytes(path)
	if err != nil {
		return err
	}
	if err := CheckDisk(path, before); err != nil {
		return err
	}
	started := time.Now()
	if _, err := legacy.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := legacy.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	after, err := logstore.FileBytes(path)
	if err != nil {
		return err
	}
	log.Info("已回收 node.db 空间", "before", before, "after", after, "duration", time.Since(started))
	if err := mark("done", map[string]any{"bytes_before": before, "bytes_after": after, "vacuum_ms": time.Since(started).Milliseconds()}); err != nil {
		return err
	}
	report("complete", "")
	return nil
}
