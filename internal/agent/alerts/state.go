package alerts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Local checkpoints are deliberately separate from the notification queue.
// They preserve an active occurrence across process restarts, not delivery receipts
// or a distributed ordering contract (those need a versioned protocol).
type savedState struct {
	Occurrence   string          `json:"occurrence"`
	Since        time.Time       `json:"since"`
	LastNotified time.Time       `json:"last_notified"`
	Confirmed    bool            `json:"confirmed"`
	Alert        json.RawMessage `json:"alert"`
}

func (e *Engine) stateFile() string { return filepath.Join(e.dir, "alerts-state.json") }

// saveStates is called while holding e.mu. A notification is checkpointed only
// after acceptance, so a crash between the state change and dispatch may duplicate
// a firing notification but cannot suppress its first attempt for four hours.
func (e *Engine) saveStates() {
	raw := map[string]savedState{}
	for key, st := range e.states {
		if !st.firing {
			continue
		}
		b, err := protojson.Marshal(st.alert)
		if err != nil {
			e.log.Error("保存告警状态失败", "err", err)
			return
		}
		raw[key] = savedState{st.occurrence, st.since, st.lastDelivered, st.confirmed, b}
	}
	data, err := json.Marshal(raw)
	if err == nil {
		err = os.WriteFile(e.stateFile()+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(e.stateFile()+".tmp", e.stateFile())
	}
	if err != nil {
		e.log.Error("保存告警状态失败", "err", err)
	}
}

func (e *Engine) loadStates() {
	data, err := os.ReadFile(e.stateFile())
	if os.IsNotExist(err) {
		return
	}
	raw := map[string]savedState{}
	if err == nil {
		err = json.Unmarshal(data, &raw)
	}
	if err != nil {
		e.log.Error("读取告警状态失败", "err", err)
		return
	}
	for key, s := range raw {
		a := &nagiv1.Alert{}
		if err := protojson.Unmarshal(s.Alert, a); err != nil || s.Occurrence == "" || s.Since.IsZero() || a.GetKey() != key || a.GetStatus() != "firing" {
			e.log.Error("读取告警状态失败", "key", key)
			continue
		}
		// On restart, live rules need new observations. Event alerts keep their
		// explicit Fire/Resolve lifecycle. Neither kind is implicitly resolved.
		e.states[key] = &state{firing: true, lastNotified: s.LastNotified, lastDelivered: s.LastNotified, alert: a,
			occurrence: s.Occurrence, since: s.Since, confirmed: s.Confirmed && isEventKey(key)}
	}
}
