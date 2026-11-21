package metrics

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/SIPC/nagi/internal/pkg/db"
)

// OpenHistory separates graph history from node configuration. Call before any
// collector starts. Copy/verify durable destination batches before deleting either
// source table; interruption can leave duplicates, never a partially deleted source.
func OpenHistory(ctx context.Context, path string, legacy *sql.DB) (*sql.DB, error) {
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
	if _, err := d.ExecContext(ctx, `PRAGMA synchronous=FULL;
		CREATE TABLE IF NOT EXISTS metrics_raw (series TEXT NOT NULL, ts INTEGER NOT NULL, value REAL NOT NULL, PRIMARY KEY(series,ts)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS metrics_m5 (series TEXT NOT NULL, ts INTEGER NOT NULL, value REAL NOT NULL, PRIMARY KEY(series,ts)) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS metrics_raw_ts ON metrics_raw(ts);
		CREATE INDEX IF NOT EXISTS metrics_m5_ts ON metrics_m5(ts);`); err != nil {
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
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return fail(err)
			}
			for _, p := range batch {
				if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES (?,?,?)`, p.series, p.ts, p.value); err == nil {
					var value float64
					err = tx.QueryRowContext(ctx, `SELECT value FROM `+table+` WHERE series=? AND ts=?`, p.series, p.ts).Scan(&value)
					if err == nil && value != p.value {
						err = fmt.Errorf("监控迁移冲突: %s %s %d", table, p.series, p.ts)
					}
				}
				if err != nil {
					tx.Rollback()
					return fail(err)
				}
			}
			if err := tx.Commit(); err != nil {
				return fail(err)
			}
			if _, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				return fail(err)
			}
			last := batch[len(batch)-1]
			series, ts, first = last.series, last.ts, false
		}
	}
	// Both source tables remain untouched until every row of both tables matches.
	tx, err := legacy.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM metrics_raw; DELETE FROM metrics_m5`); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return d, nil
}
