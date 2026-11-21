package audit

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// UseBudget keeps the old verifiable chain while sharing capacity with all other
// local history. Old databases are converted before any quota deletion.
func (l *Log) UseBudget(ctx context.Context, budget *logstore.Store, replica bool) error {
	if budget == nil {
		return nil
	}
	var mode int
	var reclaimable atomic.Bool
	// Databases written before v0.37 lack incremental auto-vacuum. Converting
	// rewrites the whole file (tens of seconds on a large chain), so it is not
	// done at startup, where it held the panel entrance; only when the capacity
	// trim first reaches the audit chain, after every lower priority is gone.
	// No record is deleted before its space can be given back.
	convert := func(ctx context.Context) error {
		n, err := logstore.FileBytes(l.path)
		if err != nil {
			return err
		}
		if err := metrics.CheckDisk(l.path, n); err != nil {
			return err
		}
		if _, err := l.db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
			return err
		}
		var m int
		if err := l.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&m); err != nil {
			return err
		}
		reclaimable.Store(m == 2)
		return nil
	}
	if err := budget.RegisterArchive(ctx, logstore.Archive{
		Name: l.path, Priority: 4, Size: func() (int64, error) { return logstore.FileBytes(l.path) },
		Trim: func(ctx context.Context) (bool, error) {
			// A replica cannot append its own prune records into the primary's ID space.
			if replica || l.historyPending.Load() {
				return false, nil
			}
			if !reclaimable.Load() {
				if err := convert(ctx); err != nil {
					return false, err
				}
				if !reclaimable.Load() {
					return false, nil
				}
			}
			var id int64
			err := l.db.QueryRowContext(ctx, `SELECT id FROM events WHERE action<>'audit_pruned' ORDER BY CASE WHEN category IN ('security','session','login','mcp') THEN 1 ELSE 0 END,id LIMIT 1`).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if err := l.deleteWhere(ctx, `id IN (SELECT id FROM events WHERE action<>'audit_pruned' AND category=(SELECT category FROM events WHERE id=?) ORDER BY id LIMIT 256)`, id); err != nil {
				return false, err
			}
			if err := logstore.CompactHistory(ctx, l.db); err != nil {
				return false, err
			}
			return true, nil
		},
	}); err != nil {
		return err
	}
	l.budget = budget
	if err := l.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	reclaimable.Store(mode == 2)
	return nil
}
