package nodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// One atomic SQLite value per authenticated node avoids partial snapshot writes.
// Terminal history is bounded; a durable snapshot floor rejects evicted deltas.
type alertLedger struct {
	Source      string               `json:"source"`
	Snapshot    uint64               `json:"snapshot"`
	Current     []*nagiv1.AlertState `json:"-"`
	History     []*nagiv1.AlertState `json:"-"`
	CurrentJSON []json.RawMessage    `json:"current"`
	HistoryJSON []json.RawMessage    `json:"history"`
}

func alertStateKey(node string) string { return "alerts/state/" + node }

func (r *Registry) loadAlertLedger(ctx context.Context, node string) (*alertLedger, error) {
	l := &alertLedger{}
	v, err := r.q.GetSetting(ctx, alertStateKey(node))
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(v), l); err != nil {
		return nil, err
	}
	for _, pair := range []struct {
		raw []json.RawMessage
		out *[]*nagiv1.AlertState
	}{{l.CurrentJSON, &l.Current}, {l.HistoryJSON, &l.History}} {
		for _, b := range pair.raw {
			a := &nagiv1.AlertState{}
			if err := protojson.Unmarshal(b, a); err != nil {
				return nil, err
			}
			*pair.out = append(*pair.out, a)
		}
	}
	l.CurrentJSON, l.HistoryJSON = nil, nil
	return l, nil
}

func (r *Registry) saveAlertLedger(ctx context.Context, node string, l *alertLedger) error {
	l.CurrentJSON, l.HistoryJSON = nil, nil
	for _, pair := range []struct {
		in  []*nagiv1.AlertState
		out *[]json.RawMessage
	}{{l.Current, &l.CurrentJSON}, {l.History, &l.HistoryJSON}} {
		for _, a := range pair.in {
			b, err := protojson.Marshal(a)
			if err != nil {
				return err
			}
			*pair.out = append(*pair.out, b)
		}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return r.q.SetSetting(ctx, store.SetSettingParams{Key: alertStateKey(node), Value: string(b)})
}

func validSourceAlert(a *nagiv1.Alert, source string, revision uint64, current bool) bool {
	if a == nil || a.GetSource() != source || a.GetRevision() == 0 || a.GetRevision() > revision || len(a.GetKey()) == 0 || len(a.GetKey()) > 800 || len(a.GetOccurrence()) == 0 || len(a.GetOccurrence()) > 100 {
		return false
	}
	if a.GetValidity() != "valid" && a.GetValidity() != "unknown" {
		return false
	}
	if current {
		return a.GetStatus() == "firing" && a.GetEndReason() == ""
	}
	return a.GetStatus() == "resolved" && a.GetEndReason() == "recovered" || a.GetStatus() == "ended" && (a.GetEndReason() == "rule_disabled" || a.GetEndReason() == "object_removed")
}

// syncAlerts is called only after authenticating/approving the source. A delta
// cannot establish or replace a source identity: first reconnect report wins.
func (r *Registry) syncAlerts(ctx context.Context, node string, s *nagiv1.AlertSync, delta bool) error {
	if s == nil {
		return nil
	}
	if s.GetSource() == "" || len(s.GetSource()) > 100 || s.GetRevision() == 0 || len(s.GetCurrent()) > 200 || len(s.GetHistory()) > 200 || proto.Size(s) > 1<<20 {
		return status.Error(codes.InvalidArgument, "告警状态超出限制")
	}
	seen := map[string]bool{}
	for _, a := range s.GetCurrent() {
		if !validSourceAlert(a, s.Source, s.Revision, true) || seen[a.GetKey()] {
			return status.Error(codes.InvalidArgument, "无效的当前告警")
		}
		seen[a.Key] = true
	}
	for _, a := range s.GetHistory() {
		if !validSourceAlert(a, s.Source, s.Revision, false) {
			return status.Error(codes.InvalidArgument, "无效的告警历史")
		}
	}
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	l, err := r.loadAlertLedger(ctx, node)
	if err != nil {
		return err
	}
	if l.Source != "" && l.Source != s.Source {
		return status.Error(codes.FailedPrecondition, "告警来源身份已改变，保留旧状态，需人工核对")
	}
	if delta && l.Source == "" {
		return status.Error(codes.FailedPrecondition, "需要先上报告警快照")
	}
	if s.Revision < l.Snapshot || delta && s.Revision <= l.Snapshot {
		return nil
	}
	l.Source = s.Source
	current := map[string]*nagiv1.AlertState{}
	for _, a := range l.Current {
		current[a.Alert.Key] = a
	}
	latest := map[string]uint64{}
	for _, a := range append(append([]*nagiv1.AlertState{}, l.History...), l.Current...) {
		latest[a.Alert.Key] = max(latest[a.Alert.Key], a.Alert.Revision)
	}
	now := timestamppb.Now()
	apply := func(a *nagiv1.Alert, active bool) {
		old := current[a.Key]
		// Equal snapshot entries refresh receipt time, but never overwrite content.
		if a.Revision <= latest[a.Key] {
			if !delta && old != nil && old.Alert.Revision == a.Revision && proto.Equal(old.Alert, a) {
				old.ReceivedAt = now
			}
			return
		}
		v := &nagiv1.AlertState{NodeId: node, Alert: proto.CloneOf(a), ReceivedAt: now}
		v.Alert.Title, v.Alert.Message = clipText(a.Title, 120), clipText(a.Message, 2000)
		v.Alert.Node, v.Alert.Project = "", "" // never trust source-supplied names as object references
		if old != nil && old.Alert.Occurrence == a.Occurrence {
			v.Acknowledged, v.Conclusion, v.SilenceUntil, v.MaintenanceUntil, v.HandlingRevision = old.Acknowledged, old.Conclusion, old.SilenceUntil, old.MaintenanceUntil, old.HandlingRevision
		} else if old != nil && active {
			// The missed terminal record may have fallen outside source history.
			// Preserve the old occurrence as unknown/ended, not recovered.
			old.Alert = proto.CloneOf(old.Alert)
			old.Alert.Status = "ended"
			old.Alert.EndReason = "superseded"
			old.Alert.Validity = "unknown"
			l.History = append(l.History, old)
		}
		latest[a.Key] = a.Revision
		if active {
			current[a.Key] = v
		} else {
			if old != nil && old.Alert.Occurrence == a.Occurrence {
				delete(current, a.Key)
			}
			l.History = append(l.History, v)
		}
	}
	// Establish current state before backfilling bounded history. An older terminal
	// for the same key must not remove a newer occurrence.
	for _, a := range s.Current {
		apply(a, true)
	}
	for _, a := range s.History {
		apply(a, false)
	}
	if !delta && s.Complete {
		for key, a := range current {
			if !seen[key] {
				a.Alert = proto.CloneOf(a.Alert)
				a.Alert.Validity = "unknown"
			}
		}
		l.Snapshot = s.Revision
	}
	l.Current = nil
	for _, a := range current {
		l.Current = append(l.Current, a)
	}
	if len(l.Current) > 200 {
		return status.Error(codes.ResourceExhausted, "当前告警已满，保留已有状态")
	}
	sort.Slice(l.History, func(i, j int) bool { return l.History[i].Alert.Revision < l.History[j].Alert.Revision })
	if len(l.History) > 200 {
		l.History = l.History[len(l.History)-200:]
	}
	if err := r.saveAlertLedger(ctx, node, l); err != nil {
		return err
	}
	r.Changes.Bump()
	return nil
}

// AlertStates returns data validity separately from detection and human handling.
// Missing source reports become stale at 90s (provisional), never resolved.
func (r *Registry) AlertStates(ctx context.Context, node string, history bool) ([]*nagiv1.AlertState, error) {
	if r.q == nil {
		return nil, nil
	}
	ids := []string{node}
	if node == "" {
		rows, err := r.q.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		ids = nil
		for _, n := range rows {
			ids = append(ids, n.ID)
		}
	}
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	var out []*nagiv1.AlertState
	for _, id := range ids {
		l, err := r.loadAlertLedger(ctx, id)
		if err != nil {
			return nil, err
		}
		list := append([]*nagiv1.AlertState{}, l.Current...)
		if history {
			list = append(list, l.History...)
		}
		for _, a := range list {
			a.Validity = a.Alert.GetValidity()
			if a.Alert.GetStatus() == "firing" && (a.ReceivedAt == nil || time.Since(a.ReceivedAt.AsTime()) > 90*time.Second) {
				a.Validity = "stale"
			}
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeId != out[j].NodeId {
			return out[i].NodeId < out[j].NodeId
		}
		return out[i].Alert.Revision > out[j].Alert.Revision
	})
	return out, nil
}

func (r *Registry) HandleAlert(ctx context.Context, req *nagiv1.HandleAlertRequest) (*nagiv1.AlertState, error) {
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	l, err := r.loadAlertLedger(ctx, req.NodeId)
	if err != nil {
		return nil, err
	}
	for _, a := range append(append([]*nagiv1.AlertState{}, l.Current...), l.History...) {
		if a.Alert.Key != req.Key || a.Alert.Occurrence != req.Occurrence {
			continue
		}
		if a.HandlingRevision != req.HandlingRevision {
			return nil, status.Error(codes.Aborted, "处理状态已改变，请刷新")
		}
		switch req.Action {
		case "acknowledge":
			a.Acknowledged = true
		case "conclusion":
			if len(req.Conclusion) > 2000 {
				return nil, status.Error(codes.InvalidArgument, "处理结论过长")
			}
			a.Conclusion = req.Conclusion
		case "silence", "maintenance":
			if req.Until == nil || req.Until.CheckValid() != nil || !req.Until.AsTime().After(time.Now()) || time.Until(req.Until.AsTime()) > 24*time.Hour {
				return nil, status.Error(codes.InvalidArgument, "需指定未来 24 小时内的结束时间")
			}
			if req.Action == "silence" {
				a.SilenceUntil = req.Until
			} else {
				a.MaintenanceUntil = req.Until
			}
		case "clear_silence":
			a.SilenceUntil = nil
		case "clear_maintenance":
			a.MaintenanceUntil = nil
		default:
			return nil, status.Error(codes.InvalidArgument, "不支持的处理动作")
		}
		a.HandlingRevision++
		if err := r.saveAlertLedger(ctx, req.NodeId, l); err != nil {
			return nil, err
		}
		r.Changes.Bump()
		return proto.CloneOf(a), nil
	}
	return nil, status.Error(codes.NotFound, "告警不存在或已超出历史保留范围")
}
