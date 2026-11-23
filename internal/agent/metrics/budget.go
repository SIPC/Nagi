package metrics

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"time"

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
		Trim: func(ctx context.Context) (bool, error) { return c.trimOldest(ctx, path) },
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
//
// It runs under the journal lock. For the background cleanup it waits for the
// history database at most trimWait and otherwise returns ErrArchiveBusy, so
// the cleanup releases the journal lock and retries instead of queueing every
// journal write behind a long chart read or a sample write.
func (c *Collector) trimOldest(ctx context.Context, path string) (bool, error) {
	background := logstore.BackgroundTrim(ctx)
	wait := ctx
	if background {
		// Wait at most trimWait for the history connection (a sample write may
		// hold it). Once held, the batch runs to completion: cancelling it would
		// roll it back and the cleanup would never progress.
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(ctx, trimWait)
		defer cancel()
	}
	conn, err := c.db.Conn(wait)
	if err != nil {
		if background && ctx.Err() == nil {
			return false, logstore.ErrArchiveBusy
		}
		return false, err
	}
	defer conn.Close()
	// A chart read in progress keeps the WAL from being reset, so each delete
	// would grow it (about 1 MB per batch) and add to the quota it is meant to
	// free. Until the read finishes, the background cleanup waits and a writer
	// frees space elsewhere.
	if wal, err := logstore.FileBytes(path + "-wal"); err == nil && wal > busyWAL(path) {
		if err := logstore.CompactHistory(ctx, conn); err != nil {
			return false, err
		}
		if wal, err := logstore.FileBytes(path + "-wal"); err == nil && wal > busyWAL(path) {
			if background {
				return false, logstore.ErrArchiveBusy
			}
			return false, nil
		}
	}
	return deleteOldest(ctx, conn)
}

// deleteOldest deletes one batch of the oldest points and reclaims their space.
func deleteOldest(ctx context.Context, conn *sql.Conn) (bool, error) {
	table, oldest := "", int64(0)
	for _, t := range []string{"metrics_raw", "metrics_m5"} {
		var ts sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT min(ts) FROM `+t).Scan(&ts); err != nil {
			return false, err
		}
		if ts.Valid && (table == "" || ts.Int64 < oldest) {
			table, oldest = t, ts.Int64
		}
	}
	if table == "" {
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM `+table+` WHERE (series,ts) IN (SELECT series,ts FROM `+table+` ORDER BY ts LIMIT ?)`, trimBatch); err != nil {
		return false, err
	}
	if err := logstore.CompactHistory(ctx, conn); err != nil {
		return false, err
	}
	return true, nil
}

// busyWAL is the WAL size above which trimming waits for chart reads to finish:
// 1/64 of the history, at most 32 MiB.
func busyWAL(path string) int64 {
	var n int64
	if st, err := os.Stat(path); err == nil {
		n = st.Size()
	}
	return min(32<<20, max(1<<20, n/64))
}

// trimWait bounds how long the background cleanup waits, holding the journal
// lock, for the history connection.
const trimWait = 500 * time.Millisecond

// closer closes the chart read connection along with history.
func (c *Collector) closer(history *sql.DB) func() error {
	return func() error {
		c.CloseReader()
		return history.Close()
	}
}

// CloseReader closes the chart read connection; the next read reopens it.
func (c *Collector) CloseReader() {
	c.readerMu.Lock()
	defer c.readerMu.Unlock()
	if c.reader != nil {
		c.reader.Close()
		c.reader, c.readerFor = nil, nil
	}
}

// read returns the connection chart reads use: a separate read-only connection
// to the separated history, so reads never wait behind sample writes or
// cleanup on the single read-write connection (SQLite WAL readers do not block
// the writer). Before separation, and if it cannot be opened, it is c.db.
// The caller holds historyMu.
func (c *Collector) read() *sql.DB {
	c.readerMu.Lock()
	defer c.readerMu.Unlock()
	if c.readerFor == c.db && c.reader != nil {
		return c.reader
	}
	if c.reader != nil {
		c.reader.Close()
		c.reader = nil
	}
	c.readerFor = c.db
	var seq int
	var name, path string
	if c.historyReadOnly || c.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path) != nil || filepath.Base(path) != "metrics.db" {
		return c.db
	}
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	r, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return c.db
	}
	r.SetMaxOpenConns(2)
	if err := r.Ping(); err != nil {
		r.Close()
		return c.db
	}
	c.reader = r
	return r
}
