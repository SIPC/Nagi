package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// OpenHistory separates graph history from node configuration. Call before any
// collector starts. Copy/verify durable destination batches before deleting either
// source table; interruption can leave duplicates, never a partially deleted source.
func OpenHistory(ctx context.Context, path string, legacy *sql.DB, budget *logstore.Store) (*sql.DB, error) {
	d, finish, err := prepareHistory(ctx, path, legacy, budget, nil)
	if err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// MigrateHistory keeps the existing collector available while copying. Only the
// final source deletion and backend handoff exclude readers. The caller owns the
// returned database closer, including when quota registration returns an error.
func (c *Collector) MigrateHistory(ctx context.Context, path string, budget *logstore.Store) (func() error, error) {
	progress := logstore.Migration{ID: "metrics", Phase: "pending"}
	report := func(p logstore.Migration) {
		progress = p
		budget.SetMigration(p)
	}
	report(progress)
	fail := func(err error) error {
		progress.Phase, progress.Error = "failed", err.Error()
		report(progress)
		return err
	}
	c.historyMu.RLock()
	legacy := c.db
	c.historyMu.RUnlock()
	history, finish, err := prepareHistory(ctx, path, legacy, budget, report)
	if err != nil {
		return nil, fail(err)
	}
	c.historyMu.Lock()
	err = finish()
	if err == nil {
		c.db, c.q = history, nodestore.New(history)
	}
	c.historyMu.Unlock()
	if err != nil {
		history.Close()
		return nil, fail(err)
	}
	if err := c.UseBudget(ctx, budget, path, true); err != nil {
		return history.Close, fail(err)
	}
	progress.Phase = "complete"
	report(progress)
	return history.Close, nil
}

// prepareHistory copies and verifies bounded batches without changing the source.
// finish commits source cleanup; the collector holds its backend lock during it.
func prepareHistory(ctx context.Context, path string, legacy *sql.DB, budget *logstore.Store, report func(logstore.Migration)) (*sql.DB, func() error, error) {
	var copied atomic.Bool
	progress := logstore.Migration{ID: "metrics", Phase: "copying"}
	var legacyPath string
	var hasLegacy bool
	if err := legacy.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metrics_raw UNION ALL SELECT 1 FROM metrics_m5)`).Scan(&hasLegacy); err != nil {
		return nil, nil, err
	}
	if report != nil {
		if err := legacy.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM metrics_raw)+(SELECT count(*) FROM metrics_m5)`).Scan(&progress.Total); err != nil {
			return nil, nil, err
		}
		report(progress)
	}
	if budget != nil {
		var seq int
		var name string
		if err := legacy.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &legacyPath); err != nil {
			return nil, nil, err
		}
		// Until verification and source deletion succeed, both copies are protected.
		// Count the entire mixed legacy file conservatively, never prune its config.
		if err := budget.RegisterArchive(ctx, logstore.Archive{Name: path, Priority: 2,
			Size: func() (int64, error) {
				n, err := logstore.FileBytes(path)
				if err != nil || copied.Load() || !hasLegacy {
					return n, err
				}
				old, err := logstore.FileBytes(legacyPath)
				return n + old, err
			}, Trim: func(context.Context) (bool, error) { return false, nil },
		}); err != nil {
			return nil, nil, err
		}
		if err := budget.WriteArchive(ctx, 2, 256<<10, func() error { return nil }); err != nil {
			return nil, nil, err
		}
	}
	d, err := db.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*sql.DB, func() error, error) { d.Close(); return nil, nil, err }
	var tables int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		return fail(err)
	}
	if tables == 0 {
		if _, err := d.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
			return fail(err)
		}
	}
	if err := budget.WriteArchive(ctx, 2, 256<<10, func() error {
		_, err := d.ExecContext(ctx, `PRAGMA synchronous=FULL;
		CREATE TABLE IF NOT EXISTS metrics_raw (series TEXT NOT NULL, ts INTEGER NOT NULL, value REAL NOT NULL, PRIMARY KEY(series,ts)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS metrics_m5 (series TEXT NOT NULL, ts INTEGER NOT NULL, value REAL NOT NULL, PRIMARY KEY(series,ts)) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS metrics_raw_ts ON metrics_raw(ts);
		CREATE INDEX IF NOT EXISTS metrics_m5_ts ON metrics_m5(ts);`)
		return err
	}); err != nil {
		return fail(err)
	}
	for _, table := range []string{"metrics_raw", "metrics_m5"} {
		var series string
		var ts int64
		first := true
		for {
			started := time.Now()
			query := `SELECT series,ts,value FROM ` + table
			var args []any
			if !first {
				// Keep the cursor predicate free of OR so SQLite seeks into the
				// timestamp index instead of rescanning every previous batch.
				query += ` WHERE (ts,series)>(?,?)`
				args = []any{ts, series}
			}
			query += ` ORDER BY ts,series LIMIT 2048`
			rows, err := legacy.QueryContext(ctx, query, args...)
			if err != nil {
				return fail(err)
			}
			type point struct {
				series string
				ts     int64
				value  float64
			}
			var batch []point
			for rows.Next() {
				var p point
				if err := rows.Scan(&p.series, &p.ts, &p.value); err != nil {
					rows.Close()
					return fail(err)
				}
				batch = append(batch, p)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return fail(err)
			}
			if len(batch) == 0 {
				break
			}
			if err := CheckDisk(path, 1<<20); err != nil {
				return fail(err)
			}
			var reserve int64 = 64 << 10
			var values strings.Builder
			args = make([]any, 0, len(batch)*3)
			for i, p := range batch {
				reserve += int64(len(p.series)+128) * 4
				if i != 0 {
					values.WriteByte(',')
				}
				values.WriteString("(?,?,?)")
				args = append(args, p.series, p.ts, p.value)
			}
			// Only the capacity check uses the shared journal lock; the batch
			// transaction runs on metrics.db alone, so a large upgrade does not
			// starve audit, alert-state and local log writes for its duration.
			if err := budget.Reserve(ctx, 2, reserve); err != nil {
				return fail(err)
			}
			if err := func() error {
				tx, err := d.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				// Write and verify the bounded batch in two statements instead of
				// preparing an insert and a lookup for every individual point.
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES `+values.String(), args...); err != nil {
					return err
				}
				var conflictSeries string
				var conflictTS int64
				err = tx.QueryRowContext(ctx, `WITH source(series,ts,value) AS (VALUES `+values.String()+`)
					SELECT source.series,source.ts FROM source LEFT JOIN `+table+` AS target
					ON target.series=source.series AND target.ts=source.ts
					WHERE target.series IS NULL OR target.value!=source.value LIMIT 1`, args...).Scan(&conflictSeries, &conflictTS)
				if err == nil {
					return fmt.Errorf("监控迁移冲突: %s %s %d", table, conflictSeries, conflictTS)
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				// Truncate only an oversized WAL (it counts against the shared
				// budget); a TRUNCATE after every 2048 rows made the copy fsync-bound.
				if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > 64<<20 {
					if _, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
						return err
					}
				}
				return nil
			}(); err != nil {
				return fail(err)
			}
			last := batch[len(batch)-1]
			series, ts, first = last.series, last.ts, false
			progress.Completed += int64(len(batch))
			if report != nil {
				report(progress)
			}
			// Yield at least as long as the batch took, with no DB/budget lock
			// held. Recovery must leave CPU and I/O time for live services.
			timer := time.NewTimer(max(time.Since(started), 20*time.Millisecond))
			select {
			case <-ctx.Done():
				timer.Stop()
				return fail(ctx.Err())
			case <-timer.C:
			}
		}
	}
	// Both source tables remain untouched until every row of both tables matches.
	var reserve int64
	if hasLegacy {
		reserve, err = logstore.FileBytes(legacyPath)
		if err != nil {
			return fail(err)
		}
	}
	return d, func() error {
		progress.Phase = "cleanup"
		if report != nil {
			report(progress)
		}
		if err := budget.WriteArchive(ctx, 2, reserve+(64<<10), func() error {
			tx, err := legacy.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `DELETE FROM metrics_raw; DELETE FROM metrics_m5`); err != nil {
				return err
			}
			return tx.Commit()
		}); err != nil {
			return err
		}
		copied.Store(true)
		return nil
	}, nil
}
