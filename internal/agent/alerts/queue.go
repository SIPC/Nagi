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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Pending delivery is an intent, not a saved message or a saved credential.
// The one exception is Legacy: a v0.36 queue entry has no occurrence to bind
// to, so its own alert text is kept and sent once to the channel the panel
// currently configures with the same ID and destination. Its saved channel
// (URL or token) is never kept.
type queued struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Occurrence  string          `json:"occurrence"`
	Key         string          `json:"key"`
	Status      string          `json:"status"`
	Panel       string          `json:"panel"`
	Channel     string          `json:"channel_id"`
	Destination string          `json:"destination"`
	Created     time.Time       `json:"created"`
	Legacy      json.RawMessage `json:"legacy_alert,omitempty"`
}

// legacyQueued is a v0.36 alerts-queue.json entry: the full alert and channel.
type legacyQueued struct {
	Alert   json.RawMessage `json:"alert"`
	Channel json.RawMessage `json:"channel"`
	Panel   string          `json:"panel"`
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
	if q.Legacy != nil {
		return e.legacyNotification(q)
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

// legacyNotification has no current-state evidence to check: it is the old
// node's own record of a notification it failed to send, replayed as is.
func (e *Engine) legacyNotification(q queued) (*nagiv1.Alert, *nagiv1.AlertChannel) {
	a := &nagiv1.Alert{}
	if protojson.Unmarshal(q.Legacy, a) != nil {
		return nil, nil
	}
	for _, ch := range e.configs[q.Panel].GetChannels() {
		if ch.GetId() == q.Channel && destination(ch) == q.Destination {
			return a, proto.CloneOf(ch)
		}
	}
	return nil, nil
}

// migrateLegacyQueue converts a v0.36 queue. An entry whose channel is no
// longer configured, or now points elsewhere, can't be sent without the stored
// credential, so it is dropped and counted in the log.
func (e *Engine) migrateLegacyQueue(data []byte) bool {
	var old []legacyQueued
	if json.Unmarshal(data, &old) != nil {
		return false
	}
	now, dropped := time.Now().UTC(), 0
	for _, l := range old {
		a, ch := &nagiv1.Alert{}, &nagiv1.AlertChannel{}
		if protojson.Unmarshal(l.Alert, a) != nil || protojson.Unmarshal(l.Channel, ch) != nil {
			dropped++
			continue
		}
		q := queued{ID: rand.Text(), Key: a.GetKey(), Status: a.GetStatus(), Panel: l.Panel, Channel: ch.GetId(), Destination: destination(ch), Created: now, Legacy: l.Alert}
		if current, _ := e.legacyNotification(q); current == nil {
			dropped++
			continue
		}
		e.queue = append(e.queue, q)
	}
	e.log.Info("已把旧版告警待发队列迁移到新格式", "migrated", len(e.queue), "dropped", dropped)
	return true
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
	// saveQueue replaces the legacy file in one rename; a crash before it
	// leaves the legacy file for the next start to migrate again.
	if err == nil && e.migrateLegacyQueue(data) {
		e.saveQueue()
		return
	}
	var saved savedQueue
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil || saved.Version != 2 {
		e.log.Warn("损坏的告警待发队列不重放；当前告警状态保留")
		e.saveQueue()
		return
	}
	for _, q := range saved.Pending {
		// Rules are unconfirmed until fresh evaluation after restart; retain the
		// reference so evaluation can validate it, never trust its old payload.
		if q.Source == e.source && q.Occurrence != "" || q.Legacy != nil {
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
		if a != nil && !e.deliver(ctx, a, ch, q.Panel, q.Legacy != nil) {
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
