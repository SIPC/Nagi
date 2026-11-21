package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// OpenHistory separates graph history from node configuration. Call before any
// collector starts. Copy/verify durable destination batches before deleting either
// source table; interruption can leave duplicates, never a partially deleted source.
func OpenHistory(ctx context.Context, path string, legacy *sql.DB, budget *logstore.Store) (*sql.DB, error) {
	var copied atomic.Bool
	succeeded := false
	defer func() {
		if !succeeded {
			copied.Store(false)
		}
	}()
	var legacyPath string
	var hasLegacy bool
	if err := legacy.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metrics_raw UNION ALL SELECT 1 FROM metrics_m5)`).Scan(&hasLegacy); err != nil {
		return nil, err
	}
	copied.Store(!hasLegacy)
	if budget != nil {
		var seq int
		var name string
		if err := legacy.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &legacyPath); err != nil {
			return nil, err
		}
		// Until verification and source deletion succeed, both copies are protected.
		// Count the entire mixed legacy file conservatively, never prune its config.
		if err := budget.RegisterArchive(ctx, logstore.Archive{Name: path, Priority: 2,
			Size: func() (int64, error) {
				n, err := logstore.FileBytes(path)
				if err != nil || copied.Load() {
					return n, err
				}
				old, err := logstore.FileBytes(legacyPath)
				return n + old, err
			}, Trim: func(context.Context) (bool, error) { return false, nil },
		}); err != nil {
			return nil, err
		}
		if err := budget.WriteArchive(ctx, 2, 256<<10, func() error { return nil }); err != nil {
			return nil, err
		}
	}
	d, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sql.DB, error) { d.Close(); return nil, err }
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
			rows, err := legacy.QueryContext(ctx, `SELECT series,ts,value FROM `+table+` WHERE ? OR (ts,series)>(?,?) ORDER BY ts,series LIMIT 256`, first, ts, series)
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
			for _, p := range batch {
				reserve += int64(len(p.series)+128) * 4
			}
			if err := budget.WriteArchive(ctx, 2, reserve, func() error {
				tx, err := d.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				for _, p := range batch {
					if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES (?,?,?)`, p.series, p.ts, p.value); err == nil {
						var value float64
						err = tx.QueryRowContext(ctx, `SELECT value FROM `+table+` WHERE series=? AND ts=?`, p.series, p.ts).Scan(&value)
						if err == nil && value != p.value {
							err = fmt.Errorf("监控迁移冲突: %s %s %d", table, p.series, p.ts)
						}
					}
					if err != nil {
						return err
					}
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				if _, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
					return err
				}
				return nil
			}); err != nil {
				return fail(err)
			}
			last := batch[len(batch)-1]
			series, ts, first = last.series, last.ts, false
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
	if err := budget.WriteArchive(ctx, 2, reserve+(64<<10), func() error {
		tx, err := legacy.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM metrics_raw; DELETE FROM metrics_m5`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// The copy is now authoritative. A later checkpoint failure must not make
		// callers fall back to the source tables we have just committed deleting.
		return nil
	}); err != nil {
		return fail(err)
	}
	copied.Store(true)
	succeeded = true
	return d, nil
}
