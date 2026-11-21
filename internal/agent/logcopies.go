package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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
	important, ordinary := a.cfg.LogConfig.CopyImportantSeconds, a.cfg.LogConfig.CopyOrdinarySeconds
	if important == 0 {
		important = 1
	}
	if ordinary == 0 {
		ordinary = 10
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	next := map[string]time.Time{}
	var warned time.Time
	for {
		if _, trusted := a.id.panel(fingerprint); trusted {
			a.mesh.mu.Lock()
			key, ok := a.mesh.panelKeys[fingerprint]
			a.mesh.mu.Unlock()
			if ok {
				cc, err := mesh.Client(a.mesh.m, key)
				if err == nil {
					client := nagiv1.NewLogCopyServiceClient(cc)
					for _, category := range []string{logstore.Audit, logstore.Security, logstore.Alert, logstore.System, logstore.Task, logstore.Runtime, logstore.Access, logstore.Container, logstore.Metrics} {
						if time.Now().Before(next[category]) {
							continue
						}
						interval, bytes := ordinary, 256<<10
						if category == logstore.Audit || category == logstore.Security || category == logstore.Alert || category == logstore.System {
							interval, bytes = important, 64<<10
						}
						next[category] = time.Now().Add(time.Duration(interval) * time.Second)
						// Grace-retained peers confer no log permission. Recheck the
						// current trust and key even on an already-open connection.
						_, trusted := a.id.panel(fingerprint)
						a.mesh.mu.Lock()
						current := a.mesh.panelKeys[fingerprint]
						a.mesh.mu.Unlock()
						if !trusted || current != key || ctx.Err() != nil {
							break
						}
						work, cancel := context.WithTimeout(ctx, 3*time.Second)
						err := copyLogCategory(work, a.cfg.Logs, client, category, bytes)
						cancel()
						if err != nil {
							if ctx.Err() == nil && time.Since(warned) >= time.Minute {
								a.log.Warn("近期日志副本尚未确认，保留节点原件，稍后经 mesh 补传", "panel", fingerprint, "err", err)
								warned = time.Now()
							}
							break
						}
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

// One bounded batch per category per tick prevents a noisy stream from
// monopolizing the sender. The panel's durable receipt is the source of truth;
// losing node memory or a response never advances an uncommitted cursor.
func copyLogCategory(ctx context.Context, logs *logstore.Store, client nagiv1.LogCopyServiceClient, category string, maxBytes int) error {
	st, err := logs.State(ctx)
	if err != nil {
		return err
	}
	r, err := client.GetLogReceipt(ctx, &nagiv1.LogReceiptQuery{Source: st.Source, Category: category})
	if err != nil {
		return err
	}
	if r.GetSource() != st.Source || r.GetCategory() != category || r.GetAfter() < 0 {
		return fmt.Errorf("面板返回了不匹配的日志确认")
	}
	page, err := logs.ReadPage(ctx, logstore.Filter{Category: category, After: r.GetAfter(), Limit: 128})
	if err != nil {
		return err
	}
	if page.Next == r.GetAfter() {
		return nil
	}
	b := &nagiv1.LogCopyBatch{Source: st.Source, Category: category, After: r.GetAfter(), Through: page.Next, WriteErrors: page.State.WriteErrors}
	size := 0
	for _, e := range page.Events {
		data, err := json.Marshal(e.Data)
		if err != nil {
			return err
		}
		entry := &nagiv1.LogEntry{Seq: e.Seq, Time: timestamppb.New(e.Time), Received: timestamppb.New(e.Received), Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, DataJson: data}
		n := proto.Size(entry) + 16
		if size+n > maxBytes && len(b.Entries) > 0 {
			b.Through = b.Entries[len(b.Entries)-1].Seq
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
	ack, err := client.PushLogs(ctx, b)
	if err != nil {
		return err
	}
	if ack.GetSource() != b.Source || ack.GetCategory() != category || ack.GetAfter() < b.Through {
		return fmt.Errorf("面板未确认本批日志")
	}
	return nil
}
