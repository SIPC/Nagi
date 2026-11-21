package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// JournalEvent keeps live capture and historical imports on the same normalized
// format. The original verifiable chain and encrypted recordings stay intact.
func JournalEvent(e Event) logstore.Event {
	category := logstore.Audit
	if e.Category == Security {
		category = logstore.Security
	} else if e.Category == Alert {
		category = logstore.Alert
	}
	return logstore.Event{Time: e.Time, Category: category, Kind: "panel." + e.Action,
		Project: e.ProjectID, Object: e.TargetID, Data: json.RawMessage(Canonical(e))}
}

// HistoryHead takes a checked snapshot before enabling live capture. A failed
// source read must not be mistaken for an empty, fully imported history.
func (l *Log) HistoryHead(ctx context.Context) (int64, error) {
	var through int64
	err := l.db.QueryRowContext(ctx, `SELECT MAX(id) FROM (
		SELECT COALESCE(MAX(id),0) AS id FROM events UNION ALL
		SELECT COALESCE(MAX(id),0) FROM tombstones)`).Scan(&through)
	return through, err
}

// MigrateHistory imports only this panel's own startup snapshot, not a standby's
// replicated primary chain. A persistent source token survives file renames but
// distinguishes a new local audit database after a role change. No old chain,
// tombstone, or encrypted recording is removed or re-signed by this migration.
// Take through=HistoryHead before enabling live capture; run one importer per Log.
func (l *Log) MigrateHistory(ctx context.Context, dest *logstore.Store, through int64) error {
	if dest == nil {
		return nil
	}
	l.historyPending.Store(true)
	var origin string
	if err := l.budget.WriteArchive(ctx, 4, 32<<10, func() error {
		if _, err := l.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key,value) VALUES ('journal_source',?)`, rand.Text()); err != nil {
			return err
		}
		return l.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='journal_source'`).Scan(&origin)
	}); err != nil {
		return err
	}
	key := "legacy.panel.audit." + origin
	b, err := dest.Checkpoint(ctx, key)
	if err != nil {
		return err
	}
	var after int64
	if len(b) != 0 {
		if err := json.Unmarshal(b, &after); err != nil {
			return err
		}
	}
	if after < 0 || after > through {
		return fmt.Errorf("面板旧审计序号已倒退，保留原库，拒绝沿用迁移位置")
	}
	gap := func(end int64) error {
		if err := dest.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.panel.audit.gap",
			Data: map[string]any{"origin": origin, "from": after + 1, "to": end, "meaning": "原库无这些编号的事件；墓碑和原始审计链仍保留，不代表每个编号原本都有事件"}}, key, end); err != nil {
			return err
		}
		after = end
		return nil
	}
	for after < through {
		rows, err := l.db.QueryContext(ctx, `SELECT `+columns+` FROM events WHERE id>? AND id<=? ORDER BY id LIMIT 64`, after, through)
		if err != nil {
			return err
		}
		var batch []Event
		for rows.Next() {
			e, err := scan(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Missing legacy IDs can be tombstones or allocation gaps; never claim
		// they prove an interval complete, or that each missing ID was an event.
		if len(batch) == 0 {
			if err := gap(through); err != nil {
				return err
			}
			break
		}
		for _, e := range batch {
			if e.ID > after+1 {
				if err := gap(e.ID - 1); err != nil {
					return err
				}
			}
			if err := dest.ImportCheckpoint(ctx, JournalEvent(e), key, e.ID); err != nil {
				return err
			}
			after = e.ID
		}
	}
	l.historyPending.Store(false)
	return nil
}
