package metrics

import (
	"context"
	"database/sql"
	"errors"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// UseBudget enables quota-gated writes only to separated history. If migration
// failed, the source remains queryable but cannot receive samples or maintenance.
func (c *Collector) UseBudget(ctx context.Context, budget *logstore.Store, path string, separated bool) error {
	c.budget = budget
	c.historyReadOnly = !separated
	if budget == nil {
		return nil
	}
	if !separated {
		return nil // OpenHistory still accounts for both protected migration files.
	}
	if err := budget.ReplaceArchive(ctx, logstore.Archive{
		Name: path, Priority: 2, Size: func() (int64, error) { return logstore.FileBytes(path) },
		Trim: func(ctx context.Context) (bool, error) {
			var table string
			err := c.db.QueryRowContext(ctx, `SELECT name FROM (SELECT 'metrics_raw' AS name,ts FROM metrics_raw UNION ALL SELECT 'metrics_m5',ts FROM metrics_m5) ORDER BY ts LIMIT 1`).Scan(&table)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if _, err := c.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE (series,ts) IN (SELECT series,ts FROM `+table+` ORDER BY ts LIMIT 256)`); err != nil {
				return false, err
			}
			if err := logstore.CompactHistory(ctx, c.db); err != nil {
				return false, err
			}
			return true, nil
		},
	}); err != nil {
		return err
	}
	return nil
}
