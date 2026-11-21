package logging

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Decisions keeps journal I/O off lease/vote deadlines. Queuing is not durable
// confirmation. Data handed to Report must not be mutated afterwards.
type Decisions struct {
	store      *logstore.Store
	mu         sync.Mutex
	closed     bool
	events     chan logstore.Event
	stop, done chan struct{}
	lost       atomic.Int64
}

func NewDecisions(store *logstore.Store, capacity int) *Decisions {
	if store == nil {
		return nil
	}
	if capacity < 1 {
		capacity = 256
	}
	d := &Decisions{store: store, events: make(chan logstore.Event, capacity), stop: make(chan struct{}), done: make(chan struct{})}
	go d.run()
	return d
}

// Report never waits for disk or for queue space. A lost record is counted.
func (d *Decisions) Report(e logstore.Event) {
	if d == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		d.lost.Add(1)
		return
	}
	select {
	case d.events <- e:
	default:
		d.lost.Add(1)
	}
}

// Close drains for at most two seconds plus the in-flight bounded write.
// It must run before the shared store closes. Strong process death can lose
// queued records; this is observation, not the persisted lease authority.
func (d *Decisions) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.stop)
	}
	d.mu.Unlock()
	<-d.done
}

func (d *Decisions) run() {
	defer close(d.done)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var warned time.Time
	write := func(parent context.Context, e logstore.Event) bool {
		ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
		defer cancel()
		if err := d.store.Record(ctx, e); err != nil {
			if time.Since(warned) >= time.Minute {
				logger.Error("租约决策日志未能落盘", "err", err)
				warned = time.Now()
			}
			return false
		}
		return true
	}
	gap := func(ctx context.Context) {
		if n := d.lost.Load(); n > 0 && write(ctx, logstore.Event{Category: logstore.System, Kind: "decisions.gap", Data: map[string]any{"lost_records": n, "history_complete": false}}) {
			d.lost.Add(-n)
		}
	}
	store := func(ctx context.Context, e logstore.Event) {
		gap(ctx)
		if !write(ctx, e) {
			d.lost.Add(1)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case e := <-d.events:
			store(context.Background(), e)
		case <-tick.C:
			gap(context.Background())
		case <-d.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for ctx.Err() == nil {
				select {
				case e := <-d.events:
					store(ctx, e)
				default:
					gap(ctx)
					return
				}
			}
			d.lost.Add(int64(len(d.events)))
			logger.Error("租约决策日志关闭时仍有未保存记录", "lost_records", d.lost.Load())
			return
		}
	}
}
