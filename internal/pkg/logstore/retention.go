package logstore

import (
	"context"
	"time"
)

// ExpireObject expires an owner's history and atomically preserves the ordinary
// category pruning envelope. Checkpoints (including rate limits) are untouched.
func (s *Store) ExpireObject(ctx context.Context, category, object string, before time.Time) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO log_pruned(category,first_seq,last_seq,count)
		SELECT category,min(seq),max(seq),count(*) FROM log_events WHERE category=? AND object=? AND received_ns<? GROUP BY category
		ON CONFLICT(category) DO UPDATE SET first_seq=min(first_seq,excluded.first_seq),last_seq=max(last_seq,excluded.last_seq),count=count+excluded.count`, category, object, before.UnixNano())
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM log_events WHERE category=? AND object=? AND received_ns<?`, category, object, before.UnixNano()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.compact(ctx)
}
