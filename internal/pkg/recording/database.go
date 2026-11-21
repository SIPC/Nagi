package recording

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// UseDatabase selects encrypted SQLite storage for new recordings. Call before
// accepting sessions. Legacy files remain readable, but are not copied, removed,
// or sent through the old audit replication/export paths by this method.
func (r *Recordings) UseDatabase(ctx context.Context, path string, budget *logstore.Store) error {
	if r.db != nil {
		return errors.New("录制数据库已打开")
	}
	d, err := db.Open(path)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	if _, err := d.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS recording_sessions (id TEXT PRIMARY KEY, started_ns INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS recording_chunks (
 recording TEXT NOT NULL REFERENCES recording_sessions(id) ON DELETE CASCADE,
 seq INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(recording,seq)
);
CREATE INDEX IF NOT EXISTS recording_age ON recording_sessions(started_ns,id);`); err != nil {
		return err
	}
	err = budget.RegisterArchive(ctx, logstore.Archive{
		Name: path, Priority: 3,
		Size: func() (int64, error) { return logstore.FileBytes(path) },
		Trim: func(ctx context.Context) (bool, error) {
			rows, err := d.QueryContext(ctx, `SELECT id FROM recording_sessions ORDER BY started_ns,id`)
			if err != nil {
				return false, err
			}
			var oldest string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					break
				}
				if _, active := r.active.Load(id); !active {
					oldest = id
					break
				}
			}
			readErr := rows.Err()
			rows.Close() // the database deliberately has one connection
			if err != nil {
				return false, err
			}
			if readErr != nil || oldest == "" {
				return false, readErr
			}
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return false, err
			}
			defer tx.Rollback()
			// Bound each cleanup transaction, including the WAL it can create.
			if _, err = tx.ExecContext(ctx, `DELETE FROM recording_chunks WHERE recording=? AND seq IN
(SELECT seq FROM recording_chunks WHERE recording=? ORDER BY seq LIMIT 4)`, oldest, oldest); err != nil {
				return false, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM recording_sessions WHERE id=? AND NOT EXISTS
(SELECT 1 FROM recording_chunks WHERE recording=?)`, oldest, oldest); err != nil {
				return false, err
			}
			if err = tx.Commit(); err != nil {
				return false, err
			}
			return true, logstore.CompactHistory(ctx, d)
		},
	})
	if err != nil {
		return err
	}
	r.db, r.budget = d, budget
	ok = true
	return nil
}

// Close releases the database after all sessions and readers have stopped.
func (r *Recordings) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *Recordings) startDatabase(id string, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return r.budget.WriteArchive(ctx, 3, 16<<10, func() error {
		_, err := r.db.ExecContext(ctx, `INSERT INTO recording_sessions(id,started_ns) VALUES(?,?)`, id, at.UnixNano())
		return err
	})
}

func (r *Recordings) putChunk(ctx context.Context, id string, seq int, sealed []byte) error {
	return r.budget.WriteArchive(ctx, 3, int64(len(sealed))*2+(32<<10), func() error {
		if _, err := r.db.ExecContext(ctx, `INSERT INTO recording_chunks(recording,seq,body) VALUES(?,?,?)`, id, seq, sealed); err != nil {
			return err
		}
		_, err := r.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		return err
	})
}

func (r *Recordings) deleteChunk(id string, seq int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return r.budget.WriteArchive(ctx, 3, 16<<10, func() error {
		_, err := r.db.ExecContext(ctx, `DELETE FROM recording_chunks WHERE recording=? AND seq=?`, id, seq)
		return err
	})
}

func (r *Recordings) chunkSequences(id string) ([]int, bool, error) {
	if r.db != nil {
		var exists int
		err := r.db.QueryRow(`SELECT 1 FROM recording_sessions WHERE id=?`, id).Scan(&exists)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, true, err
		}
		if err == nil {
			rows, err := r.db.Query(`SELECT seq FROM recording_chunks WHERE recording=? ORDER BY seq`, id)
			if err != nil {
				return nil, true, err
			}
			defer rows.Close()
			var seqs []int
			for rows.Next() {
				var seq int
				if err := rows.Scan(&seq); err != nil {
					return nil, true, err
				}
				seqs = append(seqs, seq)
			}
			return seqs, true, rows.Err()
		}
	}
	entries, err := os.ReadDir(filepath.Join(r.Dir, id[:6], id))
	if err != nil {
		return nil, false, err
	}
	var seqs []int
	for _, entry := range entries {
		if !entry.IsDir() {
			if seq, err := strconv.Atoi(entry.Name()); err == nil && seq >= 0 {
				seqs = append(seqs, seq)
			}
		}
	}
	return seqs, false, nil
}
