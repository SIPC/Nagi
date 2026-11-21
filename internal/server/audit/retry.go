package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// insertWait bounds one audit.db commit for a request handler. An fsync stall
// longer than this moves the record to the retry queue instead of dropping it.
const (
	insertWait = 10 * time.Second
	retryLimit = 4096
	retryMax   = 10 * time.Second
	closeWait  = 3 * time.Second
)

// retryable reports a slow or locked audit.db, which a later attempt can pass.
// Other errors (constraint, corruption, closed database) would fail every
// retry and block the records queued behind, so they are confirmed lost.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "SQLITE_LOCKED") || strings.Contains(msg, "SQLITE_IOERR")
}

// note shows audit.db outcomes on the logs page next to the journal's.
func (l *Log) note(failed, lost uint64, err error) {
	l.budget.NoteWriteFailure(logstore.Audit, failed, lost, fmt.Errorf("面板审计库：%w", err))
}

// queueIfBacklog keeps chain order: while older records wait, new ones queue
// behind them instead of committing first.
func (l *Log) queueIfBacklog(e Event) bool {
	l.retryMu.Lock()
	backlog := len(l.retryQ) > 0
	l.retryMu.Unlock()
	if backlog {
		l.enqueue(e)
	}
	return backlog
}

func (l *Log) enqueue(e Event) {
	l.retryOnce.Do(func() {
		l.retryWake = make(chan struct{}, 1)
		l.retryCtx, l.retryCancel = context.WithCancel(context.Background())
		l.retryDone = make(chan struct{})
		go l.retryLoop()
	})
	l.retryMu.Lock()
	if len(l.retryQ) >= retryLimit {
		l.retryMu.Unlock()
		err := errors.New("重试队列已满，记录确认丢失")
		l.log.Error("写入审计记录失败", "err", err, "action", e.Action)
		l.note(0, 1, err)
		return
	}
	l.retryQ = append(l.retryQ, e)
	l.retryMu.Unlock()
	select {
	case l.retryWake <- struct{}{}:
	default:
	}
}

// retryLoop commits queued records oldest first, backing off while audit.db
// stays slow. Each failed attempt counts as failed, not lost.
func (l *Log) retryLoop() {
	defer close(l.retryDone)
	backoff := time.Duration(0)
	for {
		l.retryMu.Lock()
		var e Event
		ok := len(l.retryQ) > 0
		if ok {
			e = l.retryQ[0]
		}
		l.retryMu.Unlock()
		if !ok {
			select {
			case <-l.retryWake:
				continue
			case <-l.retryCtx.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(l.retryCtx, insertWait)
		_, err := l.insert(ctx, &e)
		cancel()
		if err != nil {
			if l.retryCtx.Err() != nil {
				return
			}
			if !retryable(err) {
				l.log.Error("写入审计记录失败", "err", err, "action", e.Action)
				l.note(1, 1, err)
				l.retryMu.Lock()
				l.retryQ = l.retryQ[1:]
				l.retryMu.Unlock()
				continue
			}
			l.note(1, 0, err)
			backoff = min(max(2*backoff, 200*time.Millisecond), retryMax)
			select {
			case <-time.After(backoff):
			case <-l.retryCtx.Done():
				return
			}
			continue
		}
		backoff = 0
		l.retryMu.Lock()
		l.retryQ = l.retryQ[1:]
		l.retryMu.Unlock()
		l.runHooks(e)
	}
}

// stopRetry gives the queue a short chance to drain; the rest is confirmed lost.
func (l *Log) stopRetry() {
	l.retryOnce.Do(func() {}) // orders the read below after any queue start
	if l.retryDone == nil {
		return
	}
	deadline := time.Now().Add(closeWait)
	for time.Now().Before(deadline) {
		l.retryMu.Lock()
		n := len(l.retryQ)
		l.retryMu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.retryCancel()
	<-l.retryDone
	l.retryMu.Lock()
	n := len(l.retryQ)
	l.retryQ = nil
	l.retryMu.Unlock()
	if n > 0 {
		err := errors.New("关闭时仍未写入，记录确认丢失")
		l.log.Error("写入审计记录失败", "err", err, "lost", n)
		l.note(0, uint64(n), err)
	}
}
