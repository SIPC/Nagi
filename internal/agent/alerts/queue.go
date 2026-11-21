package alerts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// Pending delivery is an intent, not a saved message or a saved credential.
// Legacy queues without occurrence/source identity are deliberately not replayed.
type queued struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Occurrence  string    `json:"occurrence"`
	Key         string    `json:"key"`
	Status      string    `json:"status"`
	Panel       string    `json:"panel"`
	Channel     string    `json:"channel_id"`
	Destination string    `json:"destination"`
	Created     time.Time `json:"created"`
}

type savedQueue struct {
	Version int      `json:"version"`
	Pending []queued `json:"pending"`
}

func destination(ch *nagiv1.AlertChannel) string {
	sum := sha256.Sum256([]byte(alert.ChannelKey(ch)))
	return hex.EncodeToString(sum[:])
}

// notificationAlert requires current evidence for firing, or the latest known
// terminal record for recovery. A newer occurrence supersedes an old recovery.
// Callers hold e.mu and clone before releasing it.
func (e *Engine) notificationAlert(q queued) *nagiv1.Alert {
	if e.stateErr != nil || e.source == "" || q.Source != e.source || q.Occurrence == "" {
		return nil
	}
	if st := e.states[q.Key]; st != nil {
		if time.Now().Before(e.suppressed[q.Occurrence]) || time.Now().Before(e.mutedKeys[q.Key]) {
			return nil
		}
		if q.Status == "firing" && st.confirmed && st.occurrence == q.Occurrence && e.ruleEnabled(q.Key) {
			return st.alert
		}
		return nil
	}
	if q.Status == "resolved" {
		for i := len(e.history) - 1; i >= 0; i-- {
			a := e.history[i]
			if a.GetKey() == q.Key {
				if a.GetOccurrence() == q.Occurrence && a.GetStatus() == "resolved" {
					return a
				}
				return nil
			}
		}
	}
	return nil
}

func (e *Engine) queuedNotification(q queued) (*nagiv1.Alert, *nagiv1.AlertChannel) {
	if q.ID == "" || q.Created.IsZero() || time.Since(q.Created) > alert.RepeatInterval || q.Created.After(time.Now().Add(time.Minute)) {
		return nil, nil
	}
	for _, ch := range e.configs[q.Panel].GetChannels() {
		if ch.GetId() == q.Channel && destination(ch) == q.Destination {
			a := e.notificationAlert(q)
			if a != nil {
				return proto.CloneOf(a), proto.CloneOf(ch)
			}
		}
	}
	return nil, nil
}

func (e *Engine) enqueue(a *nagiv1.Alert, c channel) {
	q := queued{ID: rand.Text(), Source: a.GetSource(), Occurrence: a.GetOccurrence(), Key: a.GetKey(), Status: a.GetStatus(),
		Panel: c.panel, Channel: c.ch.GetId(), Destination: destination(c.ch), Created: time.Now().UTC()}
	if current, _ := e.queuedNotification(q); current == nil {
		return
	}
	// Only the latest intent per occurrence/destination survives; a recovery
	// replaces firing rather than delivering both after a long outage.
	kept := e.queue[:0]
	for _, old := range e.queue {
		if old.Panel != q.Panel || old.Channel != q.Channel || old.Key != q.Key {
			kept = append(kept, old)
		}
	}
	e.queue = append(kept, q)
	if len(e.queue) > 200 {
		e.queue = e.queue[len(e.queue)-200:]
	}
	e.saveQueue()
}

func (e *Engine) saveQueue() {
	data, err := json.Marshal(savedQueue{Version: 2, Pending: e.queue})
	if err == nil {
		err = os.WriteFile(e.queueFile()+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(e.queueFile()+".tmp", e.queueFile())
	}
	if err != nil {
		e.log.Warn("保存告警待发意图失败", "err", err)
	}
}

func (e *Engine) loadQueue() {
	data, err := os.ReadFile(e.queueFile())
	if os.IsNotExist(err) {
		return
	}
	var saved savedQueue
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil || saved.Version != 2 {
		e.log.Warn("旧版或损坏的告警待发队列不重放；当前告警状态保留")
		e.saveQueue()
		return
	}
	for _, q := range saved.Pending {
		// Rules are unconfirmed until fresh evaluation after restart; retain the
		// reference so evaluation can validate it, never trust its old payload.
		if q.Source == e.source && q.Occurrence != "" {
			e.queue = append(e.queue, q)
		}
	}
	if len(e.queue) > 200 {
		e.queue = e.queue[len(e.queue)-200:]
	}
	e.saveQueue()
}

func (e *Engine) flushQueue(ctx context.Context) {
	e.mu.Lock()
	pending := append([]queued(nil), e.queue...)
	e.mu.Unlock()
	for _, q := range pending {
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		present := false
		for _, current := range e.queue {
			present = present || current.ID == q.ID
		}
		if !present {
			e.mu.Unlock()
			continue
		}
		a, ch := e.queuedNotification(q)
		e.mu.Unlock()
		if a != nil && !e.deliver(ctx, a, ch, q.Panel) {
			continue
		}
		e.mu.Lock()
		kept := e.queue[:0]
		for _, old := range e.queue {
			if old.ID != q.ID {
				kept = append(kept, old)
			}
		}
		e.queue = kept
		e.saveQueue()
		if a != nil && a.GetStatus() == "firing" {
			outstanding := false
			for _, old := range e.queue {
				outstanding = outstanding || old.Source == q.Source && old.Occurrence == q.Occurrence
			}
			if st := e.states[q.Key]; !outstanding && st != nil && st.confirmed && st.occurrence == q.Occurrence {
				st.lastDelivered = st.lastNotified
				e.saveStates()
			}
		}
		e.mu.Unlock()
	}
}
