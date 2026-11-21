package release

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Journal retains upgrade evidence without blocking download progress callbacks.
// Finish drains the bounded queue before recording the result (and before reexec).
// "restarting" is not proof that the new executable has started successfully.
func Journal(ctx context.Context, logs *logstore.Store, interval time.Duration, from, to string) (context.Context, func(string, error)) {
	if logs == nil {
		return ctx, func(string, error) {}
	}
	if interval <= 0 {
		interval = time.Second
	}
	id, started := rand.Text(), time.Now()
	record := func(phase string, data map[string]any) {
		data["task_id"], data["from"], data["to"], data["phase"] = id, from, to, phase
		logs.Report(logstore.Event{Category: logstore.Task, Kind: "upgrade", Data: data})
	}
	record("started", map[string]any{})
	queue, done := make(chan Progress, 32), make(chan struct{})
	go func() {
		defer close(done)
		for p := range queue {
			record("progress", map[string]any{"stage": p.Stage, "download_source": p.Source, "downloaded_bytes": p.Downloaded, "total_bytes": p.Total})
		}
	}()
	var mu sync.Mutex
	var last Progress
	var observed bool
	var lastAt time.Time
	var dropped uint64
	closed := false
	previous := progressFrom(ctx)
	ctx = WithProgress(ctx, func(p Progress) {
		if previous != nil {
			previous(p)
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		now := time.Now()
		changed := !observed || p.Stage != last.Stage || p.Source != last.Source || p.Downloaded < last.Downloaded
		last, observed = p, true
		if !changed && now.Sub(lastAt) < interval && (p.Total < 0 || p.Downloaded != p.Total) {
			return
		}
		lastAt = now
		select {
		case queue <- p:
		default:
			dropped++
		}
	})
	var once sync.Once
	return ctx, func(result string, err error) {
		once.Do(func() {
			mu.Lock()
			closed = true
			close(queue)
			final, seen, lost := last, observed, dropped
			mu.Unlock()
			<-done
			data := map[string]any{"result": result, "duration_ms": time.Since(started).Milliseconds(), "dropped_progress": lost}
			if seen {
				data["stage"], data["download_source"], data["downloaded_bytes"], data["total_bytes"] = final.Stage, final.Source, final.Downloaded, final.Total
			}
			if err != nil {
				data["error"] = err.Error()
				if errors.Is(err, context.Canceled) {
					data["result"] = "canceled"
				}
			}
			record("finished", data)
		})
	}
}
