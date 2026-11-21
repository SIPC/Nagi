package alerts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type savedState struct {
	Occurrence   string          `json:"occurrence"`
	Since        time.Time       `json:"since"`
	LastNotified time.Time       `json:"last_notified"`
	Confirmed    bool            `json:"confirmed"`
	Alert        json.RawMessage `json:"alert"`
}

type checkpoint struct {
	Source     string                `json:"source"`
	Revision   uint64                `json:"revision"`
	States     map[string]savedState `json:"states"`
	History    []json.RawMessage     `json:"history"`
	Suppressed map[string]time.Time  `json:"suppressed,omitempty"`
	MutedKeys  map[string]time.Time  `json:"muted_keys,omitempty"`
}

func (e *Engine) stateFile() string { return filepath.Join(e.dir, "alerts-state.json") }

// All source versions, including terminal records, are committed before publishing.
// A damaged checkpoint must not silently reset the source/version and replay old state.
func (e *Engine) saveStates() {
	if e.source == "" {
		return
	}
	e.revision++
	raw := checkpoint{Source: e.source, Revision: e.revision, States: map[string]savedState{}, Suppressed: e.suppressed, MutedKeys: e.mutedKeys}
	for key, st := range e.states {
		a := proto.CloneOf(st.alert)
		a.Source, a.Occurrence, a.Revision = e.source, st.occurrence, e.revision
		a.Validity = "unknown"
		if st.confirmed {
			a.Validity = "valid"
		}
		// Preserve identity keys while applying the log store's free-text scrubber.
		for _, text := range []*string{&a.Title, &a.Message} {
			b, _ := json.Marshal(map[string]string{"text": *text})
			b, err := logstore.RedactJSON(b)
			if err != nil {
				e.stateErr = err
				return
			}
			var v map[string]string
			if err := json.Unmarshal(b, &v); err != nil {
				e.stateErr = err
				return
			}
			*text = v["text"]
		}
		st.alert = a
		b, err := protojson.Marshal(a)
		if err != nil {
			e.stateErr = err
			return
		}
		raw.States[key] = savedState{st.occurrence, st.since, st.lastDelivered, st.confirmed, b}
	}
	for _, a := range e.history {
		if a.Revision == 0 {
			a.Revision = e.revision
		}
		for _, text := range []*string{&a.Title, &a.Message} {
			b, _ := json.Marshal(map[string]string{"text": *text})
			b, err := logstore.RedactJSON(b)
			if err != nil {
				e.stateErr = err
				return
			}
			var v map[string]string
			if err := json.Unmarshal(b, &v); err != nil {
				e.stateErr = err
				return
			}
			*text = v["text"]
		}
		b, err := protojson.Marshal(a)
		if err != nil {
			e.stateErr = err
			return
		}
		raw.History = append(raw.History, b)
	}
	data, err := json.Marshal(raw)
	if err == nil && e.logs == nil {
		err = errors.New("告警日志库没有启动")
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = e.logs.SaveAlertLedger(ctx, "agent", data, logstore.Event{Kind: "alert.source_checkpoint", Data: map[string]any{"source": e.source, "revision": e.revision, "current": len(e.states), "history": len(e.history)}})
	}
	e.stateErr = err
	if err != nil {
		e.log.Error("保存告警状态失败", "err", err)
	}
}

func (e *Engine) loadStates() {
	if e.logs == nil {
		e.stateErr = errors.New("告警日志库没有启动")
		return
	}
	data, err := e.logs.Checkpoint(context.Background(), "alerts/state/agent")
	if err == nil && len(data) == 0 {
		data, err = os.ReadFile(e.stateFile()) // one-way import; legacy file remains read-only
	}
	if os.IsNotExist(err) || err == nil && len(data) == 0 {
		e.source = rand.Text()
		e.saveStates()
		return
	}
	raw := checkpoint{}
	if err == nil {
		err = json.Unmarshal(data, &raw)
	}
	if err != nil {
		e.stateErr = err
		e.log.Error("读取告警状态失败", "err", err)
		return
	}
	if raw.Source == "" {
		// Upgrade the previous active-only checkpoint without inventing recovery.
		if err = json.Unmarshal(data, &raw.States); err != nil {
			e.stateErr = err
			return
		}
		raw.Source = rand.Text()
	}
	e.suppressed, e.mutedKeys = raw.Suppressed, raw.MutedKeys
	e.source, e.revision = raw.Source, raw.Revision
	for key, s := range raw.States {
		a := &nagiv1.Alert{}
		if err := protojson.Unmarshal(s.Alert, a); err != nil || s.Occurrence == "" || s.Since.IsZero() || a.GetKey() != key || a.GetStatus() != "firing" {
			e.source = "" // fail closed rather than publishing a partial checkpoint
			return
		}
		e.states[key] = &state{firing: true, lastNotified: s.LastNotified, lastDelivered: s.LastNotified, alert: a, occurrence: s.Occurrence, since: s.Since, confirmed: s.Confirmed && isEventKey(key)}
	}
	for _, b := range raw.History {
		a := &nagiv1.Alert{}
		if err := protojson.Unmarshal(b, a); err != nil {
			e.source = ""
			return
		}
		e.history = append(e.history, a)
	}
	e.saveStates() // restart invalidates live rule evidence, with a new durable revision
}

// Snapshot is independent of sending and channel configuration. Full current
// state is resent every report; bounded terminal history repairs missed deltas.
func (e *Engine) Snapshot() *nagiv1.AlertSync {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.source == "" || e.stateErr != nil {
		return nil
	}
	s := &nagiv1.AlertSync{Source: e.source, Revision: e.revision, Complete: true}
	for _, st := range e.states {
		s.Current = append(s.Current, proto.CloneOf(st.alert))
	}
	for _, a := range e.history {
		s.History = append(s.History, proto.CloneOf(a))
	}
	return s
}

// ApplyHandling does not change detection or manufacture recovery. Only the
// currently accepted primary's report response reaches this method.
func (e *Engine) ApplyHandling(states []*nagiv1.AlertState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	suppressed := map[string]time.Time{}
	now := time.Now()
	if e.mutedKeys == nil {
		e.mutedKeys = map[string]time.Time{}
	}
	changed := false
	for _, s := range states {
		if s.GetAlert().GetSource() != e.source {
			continue
		}
		until := s.GetSilenceUntil().AsTime()
		if t := s.GetMaintenanceUntil().AsTime(); t.After(until) {
			until = t
		}
		key := s.GetAlert().GetKey()
		if until.After(now) && until.Sub(now) <= 24*time.Hour {
			suppressed[s.Alert.Occurrence] = until
			// 按键记下：告警反复出现时，新一轮也不通知（见 docs/monitoring.md#告警）。
			if !until.Equal(e.mutedKeys[key]) {
				e.mutedKeys[key], changed = until, true
			}
		} else if st := e.states[key]; st != nil && st.occurrence == s.Alert.Occurrence && !e.mutedKeys[key].IsZero() {
			delete(e.mutedKeys, key) // 当前这一轮的静默被取消了
			changed = true
		}
	}
	for k, t := range e.mutedKeys {
		if !t.After(now) {
			delete(e.mutedKeys, k)
			changed = true
		}
	}
	changed = changed || len(suppressed) != len(e.suppressed)
	for k, v := range suppressed {
		if !v.Equal(e.suppressed[k]) {
			changed = true
		}
	}
	if changed {
		e.suppressed = suppressed
		e.saveStates()
	}
}
