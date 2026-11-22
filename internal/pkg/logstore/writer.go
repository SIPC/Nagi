package logstore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Report queues are bounded by entries and encoded bytes. Important records
// (audit, security, alert, logstore) are retried until committed and are only
// lost when their queue overflows or the process dies before the commit.
// Ordinary records get one batched attempt; a failed batch is counted as lost.
const (
	queueEntries  = 8192
	queueBytes    = 16 << 20
	batchEntries  = 512
	batchBytes    = 1 << 20
	retryMax      = 10 * time.Second
	lostEverySecs = 10
)

// WriteFailure is this process's per-category write outcome. Failed counts
// attempts (a retried important batch counts each try); Lost counts records
// that were dropped and will never be written.
type WriteFailure struct {
	Category  string    `json:"category"`
	Failed    uint64    `json:"failed"`
	Lost      uint64    `json:"lost"`
	LastError string    `json:"last_error"`
	LastTime  time.Time `json:"last_time"`
}

type row struct {
	e    Event
	p    int
	data []byte
}

func (r row) size() int64 {
	return int64(len(r.data)+len(r.e.Kind)+len(r.e.Project)+len(r.e.Object)) + 256
}

type queue struct {
	items []row
	bytes int64
}

type writer struct {
	mu        sync.Mutex
	wake      chan struct{}
	queues    [2]queue // 0 ordinary, 1 important
	inflight  int
	closed    bool
	stop      chan struct{}
	done      chan struct{}
	start     sync.Once
	failures  map[string]*WriteFailure
	lostDirty atomic.Bool
}

func important(category string) bool {
	switch category {
	case Audit, Security, Alert, System:
		return true
	}
	return false
}

func (s *Store) fail(category string, failed, lost uint64, err error) {
	if failed == 0 && lost == 0 {
		return
	}
	// A dropped record is an error even if it never reached SQLite. Failed
	// and Lost can describe the same record, so do not count their sum.
	s.errors.Add(max(failed, lost))
	w := &s.w
	w.mu.Lock()
	if w.failures == nil {
		w.failures = map[string]*WriteFailure{}
	}
	f := w.failures[category]
	if f == nil {
		f = &WriteFailure{Category: category}
		w.failures[category] = f
	}
	f.Failed += failed
	f.Lost += lost
	if err != nil {
		f.LastError = clipText(err.Error(), 300)
	}
	f.LastTime = time.Now().UTC()
	w.mu.Unlock()
	if lost > 0 {
		w.lostDirty.Store(true)
	}
	now, prev := time.Now().Unix(), s.lastError.Load()
	if now-prev >= 60 && s.lastError.CompareAndSwap(prev, now) {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("本地日志入库失败", "category", category, "lost", lost, "err", err)
	}
}

// NoteWriteFailure records the outcome of a store outside logs.db, such as the
// panel's legacy audit chain, so the logs page shows it next to the journal's.
func (s *Store) NoteWriteFailure(category string, failed, lost uint64, err error) {
	if s != nil {
		s.fail(category, failed, lost, err)
	}
}

// WriteFailures returns a stable snapshot ordered by category.
func (s *Store) WriteFailures() []WriteFailure {
	if s == nil {
		return nil
	}
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	out := make([]WriteFailure, 0, len(s.w.failures))
	for _, f := range s.w.failures {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	return out
}

// Report never waits for the disk writer. It is not a durable confirmation:
// use Record when the caller needs to know the event was committed.
func (s *Store) Report(e Event) {
	s.ReportBatch([]Event{e})
}

// ReportBatch queues events atomically: all of them or none, so readers never
// see half of a multi-part sample. Events of one batch must share importance.
func (s *Store) ReportBatch(events []Event) {
	if s == nil || len(events) == 0 {
		return
	}
	rows := make([]row, 0, len(events))
	var size int64
	for _, e := range events {
		r, err := prepareRow(e)
		if err != nil {
			s.fail(e.Category, 1, 1, err)
			continue
		}
		rows = append(rows, r)
		size += r.size()
	}
	if len(rows) == 0 {
		return
	}
	k := 0
	if important(rows[0].e.Category) {
		k = 1
	}
	w := &s.w
	w.start.Do(func() {
		w.wake, w.stop, w.done = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		go s.run()
	})
	w.mu.Lock()
	q := &w.queues[k]
	if w.closed || len(q.items)+len(rows) > queueEntries || q.bytes+size > queueBytes {
		w.mu.Unlock()
		s.fail(rows[0].e.Category, 0, uint64(len(rows)), errors.New("日志写入队列已满或已关闭"))
		return
	}
	q.items = append(q.items, rows...)
	q.bytes += size
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take removes one bounded batch, important records first.
func (w *writer) take() ([]row, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range []int{1, 0} {
		q := &w.queues[k]
		if len(q.items) == 0 {
			continue
		}
		n, size := 0, int64(0)
		for n < len(q.items) && n < batchEntries && (n == 0 || size+q.items[n].size() <= batchBytes) {
			size += q.items[n].size()
			n++
		}
		batch := q.items[:n:n]
		q.items, q.bytes = q.items[n:], q.bytes-size
		w.inflight++
		return batch, k
	}
	return nil, 0
}

func (w *writer) requeue(batch []row) {
	w.mu.Lock()
	defer w.mu.Unlock()
	q := &w.queues[1]
	var size int64
	for _, r := range batch {
		size += r.size()
	}
	q.items = append(append([]row(nil), batch...), q.items...)
	q.bytes += size
}

func (s *Store) run() {
	w := &s.w
	defer close(w.done)
	backoff := time.Duration(0)
	lostTick := time.NewTicker(lostEverySecs * time.Second)
	defer lostTick.Stop()
	for {
		select {
		case <-lostTick.C:
			s.reportLost()
		default:
		}
		batch, k := w.take()
		if batch == nil {
			w.mu.Lock()
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-w.wake:
			case <-lostTick.C:
				s.reportLost()
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.writeRows(ctx, batch)
		cancel()
		w.mu.Lock()
		w.inflight--
		closed := w.closed
		w.mu.Unlock()
		if err == nil {
			backoff = 0
			continue
		}
		counts := map[string]uint64{}
		for _, r := range batch {
			counts[r.e.Category]++
		}
		if k == 0 || closed || errors.Is(err, sql.ErrConnDone) {
			for c, n := range counts {
				s.fail(c, n, n, err)
			}
			continue
		}
		for c, n := range counts {
			s.fail(c, n, 0, err)
		}
		w.requeue(batch)
		backoff = min(max(2*backoff, 200*time.Millisecond), retryMax)
		select {
		case <-time.After(backoff):
		case <-w.stop:
		}
	}
}

// reportLost persists a bounded summary of confirmed losses so the gap is
// visible after restarts and on panels holding recent copies.
func (s *Store) reportLost() {
	if !s.w.lostDirty.Swap(false) {
		return
	}
	lost := map[string]uint64{}
	for _, f := range s.WriteFailures() {
		if f.Lost > 0 {
			lost[f.Category] = f.Lost
		}
	}
	s.Report(Event{Category: System, Kind: "logstore.write.lost", Data: map[string]any{"lost_by_category": lost, "history_complete": false,
		"reason": "本进程启动以来确认未写入的记录累计数；队列溢出或写入失败"}})
}

// Flush waits until every record queued before the call is committed or
// counted as lost. Tests and orderly shutdown use it.
func (s *Store) Flush(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for {
		s.w.mu.Lock()
		idle := len(s.w.queues[0].items) == 0 && len(s.w.queues[1].items) == 0 && s.w.inflight == 0
		s.w.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// stopWriter drains for at most two seconds; the rest is counted as lost.
func (s *Store) stopWriter() {
	w := &s.w
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Flush(ctx)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	var rest []row
	for k := range w.queues {
		rest = append(rest, w.queues[k].items...)
		w.queues[k] = queue{}
	}
	started := w.done != nil
	w.mu.Unlock()
	for _, r := range rest {
		s.fail(r.e.Category, 0, 1, errors.New("关闭时未能写入"))
	}
	if started {
		close(w.stop)
		select {
		case w.wake <- struct{}{}:
		default:
		}
		<-w.done
	}
}
