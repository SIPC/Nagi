package metrics

import (
	"context"
	"database/sql"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// UseBudget enables quota-gated writes only to separated history. Until the
// destination is ready, the source remains queryable but never receives samples.
func (c *Collector) UseBudget(ctx context.Context, budget *logstore.Store, path string, separated bool) error {
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	c.budget = budget
	c.historyReadOnly = true
	if budget == nil {
		c.historyReadOnly = !separated
		return nil
	}
	if !separated {
		return nil // OpenHistory still accounts for both protected migration files.
	}
	if err := budget.ReplaceArchive(ctx, logstore.Archive{
		Name: path, Priority: 2, Size: func() (int64, error) { return logstore.FileBytes(path) },
		Trim: c.trimOldest,
	}); err != nil {
		return err
	}
	c.historyReadOnly = false
	return nil
}

// trimBatch is how many of the oldest points one Trim removes. The log store
// calls Trim under its write lock until usage is back under the line, so each
// call must make real progress: 256 points per call could not keep up once
// a node's chart history alone filled the quota. 2048 points take about
// 150 ms on a 25M-point history, short enough to hold the gate.
const trimBatch = 2048

// trimOldest deletes the oldest batch from whichever table holds the oldest
// point. Each table's oldest point comes from its ts index (two lookups),
// instead of sorting both tables, which took seconds on a large history.
func (c *Collector) trimOldest(ctx context.Context) (bool, error) {
	table, oldest := "", int64(0)
	for _, t := range []string{"metrics_raw", "metrics_m5"} {
		var ts sql.NullInt64
		if err := c.db.QueryRowContext(ctx, `SELECT min(ts) FROM `+t).Scan(&ts); err != nil {
			return false, err
		}
		if ts.Valid && (table == "" || ts.Int64 < oldest) {
			table, oldest = t, ts.Int64
		}
	}
	if table == "" {
		return false, nil
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE (series,ts) IN (SELECT series,ts FROM `+table+` ORDER BY ts LIMIT ?)`, trimBatch); err != nil {
		return false, err
	}
	if err := logstore.CompactHistory(ctx, c.db); err != nil {
		return false, err
	}
	return true, nil
}
