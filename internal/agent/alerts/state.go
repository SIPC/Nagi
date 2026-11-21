package alerts

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
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
	Source   string                `json:"source"`
	Revision uint64                `json:"revision"`
	States   map[string]savedState `json:"states"`
	History  []json.RawMessage     `json:"history"`
}

func (e *Engine) stateFile() string { return filepath.Join(e.dir, "alerts-state.json") }

// All source versions, including terminal records, are committed before publishing.
// A damaged checkpoint must not silently reset the source/version and replay old state.
func (e *Engine) saveStates() {
	if e.source == "" {
		return
	}
	e.revision++
	raw := checkpoint{Source: e.source, Revision: e.revision, States: map[string]savedState{}}
	for key, st := range e.states {
		a := proto.CloneOf(st.alert)
		a.Source, a.Occurrence, a.Revision = e.source, st.occurrence, e.revision
		a.Validity = "unknown"
		if st.confirmed {
			a.Validity = "valid"
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
		b, err := protojson.Marshal(a)
		if err != nil {
			e.stateErr = err
			return
		}
		raw.History = append(raw.History, b)
	}
	data, err := json.Marshal(raw)
	if err == nil {
		err = os.WriteFile(e.stateFile()+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(e.stateFile()+".tmp", e.stateFile())
	}
	e.stateErr = err
	if err != nil {
		e.log.Error("保存告警状态失败", "err", err)
	}
}

func (e *Engine) loadStates() {
	data, err := os.ReadFile(e.stateFile())
	if os.IsNotExist(err) {
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

func (e *Engine) ruleEnabled(key string, fallback judge) bool {
	kind, rest, _ := strings.Cut(key, ":")
	if kind == "event" {
		kind, rest, _ = strings.Cut(rest, ":")
		switch kind {
		case "cert_failed":
			return fallback.on(alert.RuleCert)
		case "snapshot", "maintain":
			p, c, ok := strings.Cut(rest, "/")
			if ok {
				return e.forContainer(p, c, fallback).on(alert.RuleSnapshot)
			}
			return fallback.on(alert.RuleSnapshot)
		case "backup", "standby":
			return fallback.on(alert.RuleBackup)
		default:
			return true
		}
	}
	if kind == "cert" && e.Domains != nil && !e.Domains()[rest] {
		return true
	} // absence alone isn't health
	if kind == "errors" || kind == "4xx" || kind == "traffic" {
		p, r := splitContainer(rest)
		if e.RouteRules != nil {
			rules, ok := e.RouteRules(p, r)
			return forRules(rules, ok, fallback).on(kind)
		}
	}
	if kind == "health" || kind == "restart" || kind == "container_cpu" {
		p, c := splitContainer(rest)
		c, _, _ = strings.Cut(c, "/")
		return e.forContainer(p, c, fallback).on(kind)
	}
	if kind == alert.RuleDisk || kind == alert.RuleMem || kind == alert.RuleCPU || kind == alert.RuleCert {
		return fallback.on(kind)
	}
	return true
}
