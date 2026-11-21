package agent

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (a *agent) logCopiesLoop(ctx context.Context) {
	if a.mesh == nil || a.cfg.Logs == nil {
		return
	}
	// A dead primary must not serialize or delay the standby's uploads.
	workers := map[string]context.CancelFunc{}
	var done sync.WaitGroup
	defer func() {
		for _, cancel := range workers {
			cancel()
		}
		done.Wait()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		panels := map[string]bool{}
		for _, p := range a.id.Panels() {
			fp := p.Fingerprint
			panels[fp] = true
			if workers[fp] == nil {
				work, cancel := context.WithCancel(ctx)
				workers[fp] = cancel
				done.Add(1)
				go func() {
					defer done.Done()
					a.logCopiesToPanel(work, fp)
				}()
			}
		}
		for fp, cancel := range workers {
			if !panels[fp] {
				cancel()
				delete(workers, fp)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *agent) logCopiesToPanel(ctx context.Context, fingerprint string) {
	c := a.cfg.LogConfig
	defaults := logstore.DefaultConfig()
	if c.CopyCatchupEntries == 0 { // Tests and old configs constructed in code.
		c.CopyCatchupEntries, c.CopyCatchupKiB, c.CopyBackoffMaxSeconds, c.CopyYieldPressurePercent = defaults.CopyCatchupEntries, defaults.CopyCatchupKiB, defaults.CopyBackoffMaxSeconds, defaults.CopyYieldPressurePercent
	}
	if c.CopyImportantSeconds == 0 {
		c.CopyImportantSeconds = 1
	}
	if c.CopyOrdinarySeconds == 0 {
		c.CopyOrdinarySeconds = 10
	}
	budget := a.copyBudget()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	next := map[string]time.Time{}
	behind := map[string]bool{}
	var warned, retryAt, catchupAt time.Time
	var failures, sheds int
	for {
		if _, trusted := a.id.panel(fingerprint); trusted && !time.Now().Before(retryAt) {
			a.mesh.mu.Lock()
			key, ok := a.mesh.panelKeys[fingerprint]
			a.mesh.mu.Unlock()
			if ok {
				cc, err := mesh.Client(a.mesh.m, key)
				if err == nil {
					client := nagiv1.NewLogCopyServiceClient(cc)
					// Pressure is sampled once per round: yielding is coarse and cheap.
					yield := a.copyPressure() >= float64(c.CopyYieldPressurePercent)
					for _, category := range []string{logstore.Audit, logstore.Security, logstore.Alert, logstore.System, logstore.Task, logstore.Runtime, logstore.Access, logstore.Container, logstore.Metrics} {
						important := category == logstore.Audit || category == logstore.Security || category == logstore.Alert || category == logstore.System
						interval, entries, bytes := c.CopyOrdinarySeconds, 128, 256<<10
						if important {
							interval, bytes = c.CopyImportantSeconds, 64<<10
						}
						now := time.Now()
						// A category still behind after its last batch may send one
						// larger batch per second, inside this node's shared budget,
						// while neither side is under pressure. Its normal batch is
						// never withheld, so catch-up only ever adds throughput.
						catchup := behind[category] && !yield && !now.Before(catchupAt) && budget.ready(important)
						if catchup {
							entries, bytes = c.CopyCatchupEntries, c.CopyCatchupKiB<<10
						} else if now.Before(next[category]) {
							continue
						}
						// Grace-retained peers confer no log permission. Recheck the
						// current trust and key even on an already-open connection.
						_, trusted := a.id.panel(fingerprint)
						a.mesh.mu.Lock()
						current := a.mesh.panelKeys[fingerprint]
						a.mesh.mu.Unlock()
						if !trusted || current != key || ctx.Err() != nil {
							break
						}
						due := next[category]
						next[category] = now.Add(time.Duration(interval) * time.Second)
						work, cancel := context.WithTimeout(ctx, 3*time.Second)
						sent, err := copyLogCategory(work, a.cfg.Logs, client, category, entries, bytes, catchup)
						cancel()
						if sent.size > 0 {
							budget.charge(important, sent.entries, sent.size)
						}
						if status.Code(err) == codes.ResourceExhausted && catchup {
							// The panel shed catch-up to protect its own writes.
							// The normal schedule is kept; only catch-up backs off.
							next[category] = due
							sheds++
							catchupAt = time.Now().Add(copyBackoff(sheds, c.CopyBackoffMaxSeconds))
							continue
						}
						if err != nil {
							failures++
							retryAt = time.Now().Add(copyBackoff(failures, c.CopyBackoffMaxSeconds))
							clear(behind)
							if ctx.Err() == nil && time.Since(warned) >= time.Minute {
								a.log.Warn("近期日志副本尚未确认，保留节点原件，稍后经 mesh 补传", "panel", fingerprint, "err", err)
								warned = time.Now()
							}
							break
						}
						failures = 0
						if catchup {
							sheds = 0
						}
						behind[category] = sent.more
					}
					cc.Close()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// copyBackoff doubles from one second up to maxSeconds, with ±20% jitter so
// many nodes recovering together do not retry in lockstep.
func copyBackoff(attempt, maxSeconds int) time.Duration {
	d := time.Duration(maxSeconds) * time.Second
	if attempt < 30 {
		d = min(d, time.Second<<(attempt-1))
	}
	return d + time.Duration((mrand.Float64()*0.4-0.2)*float64(d))
}

type copyState struct {
	once     sync.Once
	budget   *copyBudget
	mu       sync.Mutex
	at       time.Time
	pressure float64
	psi      func() float64 // replaceable for tests
}

// copyBudget is shared by every panel's sender on this node.
func (a *agent) copyBudget() *copyBudget {
	a.copies.once.Do(func() {
		c := a.cfg.LogConfig
		if c.CopyBudgetKiBPerSecond == 0 {
			d := logstore.DefaultConfig()
			c.CopyBudgetKiBPerSecond, c.CopyBudgetEntriesPerSec = d.CopyBudgetKiBPerSecond, d.CopyBudgetEntriesPerSec
		}
		a.copies.budget = newCopyBudget(c.CopyBudgetKiBPerSecond<<10, c.CopyBudgetEntriesPerSec, time.Now)
	})
	return a.copies.budget
}

// copyPressure is cached for a second; every panel worker reads it each round.
func (a *agent) copyPressure() float64 {
	a.copies.mu.Lock()
	defer a.copies.mu.Unlock()
	if time.Since(a.copies.at) >= time.Second {
		read := a.copies.psi
		if read == nil {
			read = metrics.Pressure
		}
		a.copies.pressure, a.copies.at = read(), time.Now()
	}
	return a.copies.pressure
}

type copySent struct {
	entries, size int
	more          bool
}

// One bounded batch per call prevents a noisy stream from monopolizing the
// sender. The panel's durable receipt is the source of truth; losing node
// memory or a response never advances an uncommitted cursor.
func copyLogCategory(ctx context.Context, logs *logstore.Store, client nagiv1.LogCopyServiceClient, category string, maxEntries, maxBytes int, catchup bool) (copySent, error) {
	var sent copySent
	st, err := logs.State(ctx)
	if err != nil {
		return sent, err
	}
	r, err := client.GetLogReceipt(ctx, &nagiv1.LogReceiptQuery{Source: st.Source, Category: category})
	if err != nil {
		return sent, err
	}
	if r.GetSource() != st.Source || r.GetCategory() != category || r.GetAfter() < 0 {
		return sent, fmt.Errorf("面板返回了不匹配的日志确认")
	}
	page, err := logs.ReadPage(ctx, logstore.Filter{Category: category, After: r.GetAfter(), Limit: maxEntries})
	if err != nil {
		return sent, err
	}
	if page.Next == r.GetAfter() {
		return sent, nil
	}
	b := &nagiv1.LogCopyBatch{Source: st.Source, Category: category, After: r.GetAfter(), Through: page.Next, WriteErrors: page.State.WriteErrors, Catchup: catchup}
	sent.more = page.More
	size := 0
	for _, e := range page.Events {
		data, err := json.Marshal(e.Data)
		if err != nil {
			return sent, err
		}
		entry := &nagiv1.LogEntry{Seq: e.Seq, Time: timestamppb.New(e.Time), Received: timestamppb.New(e.Received), Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, DataJson: data}
		n := proto.Size(entry) + 16
		if size+n > maxBytes && len(b.Entries) > 0 {
			b.Through = b.Entries[len(b.Entries)-1].Seq
			sent.more = true
			break
		}
		b.Entries = append(b.Entries, entry)
		size += n
	}
	for _, p := range page.State.Pruned {
		if p.Category == category && p.Last <= b.Through {
			b.Pruned = append(b.Pruned, &nagiv1.LogPruned{Category: p.Category, First: p.First, Last: p.Last, Count: p.Count})
		}
	}
	sent.entries, sent.size = len(b.Entries), proto.Size(b)
	ack, err := client.PushLogs(ctx, b)
	if err != nil {
		return sent, err
	}
	if ack.GetSource() != b.Source || ack.GetCategory() != category || ack.GetAfter() < b.Through {
		return sent, fmt.Errorf("面板未确认本批日志")
	}
	return sent, nil
}
