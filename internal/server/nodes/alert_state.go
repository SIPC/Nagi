package nodes

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
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
	Term        uint64               `json:"term"`
	Revision    uint64               `json:"revision"`
	Source      string               `json:"source"`
	Snapshot    uint64               `json:"snapshot"`
	Current     []*nagiv1.AlertState `json:"-"`
	History     []*nagiv1.AlertState `json:"-"`
	CurrentJSON []json.RawMessage    `json:"current"`
	HistoryJSON []json.RawMessage    `json:"history"`
	// Baselines 是节点最近一次完整快照里的基线进度（protojson 的 AlertBaseline），BaselineAt 是收到的时间（Unix 秒）；
	// 节点不上报（v0.37）时为空，界面上提示升级。
	Baselines  []json.RawMessage `json:"baselines,omitempty"`
	BaselineAt int64             `json:"baseline_at,omitempty"`
}

func alertStateKey(node string) string { return "alerts/state/" + node }

// maxBaselines 是每个节点最多记几条基线进度（节点不可信，不能无限制地写）。
const maxBaselines = 500

// AlertBaselines 返回节点最近一次上报的基线进度和收到的时间；reported 为 false 表示节点从没上报过（旧版本）。
func (r *Registry) AlertBaselines(ctx context.Context, node string) (list []*nagiv1.AlertBaseline, at time.Time, reported bool, err error) {
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	l, err := r.loadAlertLedger(ctx, node)
	if err != nil || l.BaselineAt == 0 {
		return nil, time.Time{}, false, err
	}
	for _, raw := range l.Baselines {
		b := &nagiv1.AlertBaseline{}
		if protojson.Unmarshal(raw, b) == nil {
			list = append(list, b)
		}
	}
	return list, time.Unix(l.BaselineAt, 0), true, nil
}

func (r *Registry) loadAlertLedger(ctx context.Context, node string) (*alertLedger, error) {
	l := &alertLedger{}
	if r.AlertLogs == nil {
		return nil, status.Error(codes.Unavailable, "告警日志库没有启动")
	}
	v, err := r.AlertLogs.Checkpoint(ctx, alertStateKey(node))
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return l, nil
	}
	if err = json.Unmarshal(v, l); err != nil {
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

func (r *Registry) saveAlertLedger(ctx context.Context, node string, l *alertLedger, event logstore.Event) error {
	if r.Cluster != nil {
		l.Term = r.Cluster(ctx).GetTerm()
	}
	l.Revision++
	l.CurrentJSON, l.HistoryJSON = nil, nil
	for _, pair := range []struct {
		in  []*nagiv1.AlertState
		out *[]json.RawMessage
	}{{l.Current, &l.CurrentJSON}, {l.History, &l.HistoryJSON}} {
		for _, a := range pair.in {
			for _, text := range []*string{&a.Alert.Title, &a.Alert.Message, &a.Conclusion} {
				raw, _ := json.Marshal(map[string]string{"text": *text})
				b, err := logstore.RedactJSON(raw)
				if err != nil {
					return err
				}
				var v struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(b, &v); err != nil {
					return err
				}
				*text = v.Text
			}
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
	return r.AlertLogs.SaveAlertLedger(ctx, node, b, event)
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
	return a.GetStatus() == "resolved" && a.GetEndReason() == "recovered" || a.GetStatus() == "ended" && (a.GetEndReason() == "rule_disabled" || a.GetEndReason() == "object_removed" || a.GetEndReason() == "insufficient_data")
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
		latest[a.Alert.Key+"\x00"+a.Alert.Occurrence] = max(latest[a.Alert.Key+"\x00"+a.Alert.Occurrence], a.Alert.Revision)
	}
	now := timestamppb.Now()
	apply := func(a *nagiv1.Alert, active bool) {
		old := current[a.Key]
		// Equal snapshot entries refresh receipt time, but never overwrite content.
		if a.Revision <= latest[a.Key+"\x00"+a.Occurrence] {
			if !delta && old != nil && old.Alert.Occurrence == a.Occurrence && old.Alert.Revision == a.Revision {
				old.ReceivedAt = now
			}
			return
		}
		v := &nagiv1.AlertState{NodeId: node, Alert: proto.CloneOf(a), ReceivedAt: now}
		v.Alert.Title, v.Alert.Message = clipText(a.Title, 120), clipText(a.Message, 2000)
		v.Alert.Node, v.Alert.Project = "", "" // never trust source-supplied names as object references
		if project, container := alertContainer(a.Key); container != "" {
			kind, _, _ := strings.Cut(a.Key, ":")
			if d, ok := alert.DefOf(kind); ok && d.Route {
				container = ""
				// 键是 <规则>:<项目>/<映射>，带宽还有 /in、/out。
				name, _, _ := strings.Cut(strings.TrimPrefix(a.Key, kind+":"+project+"/"), "/")
				if routes, err := r.q.ListRoutes(ctx, project); err == nil {
					for _, route := range routes {
						if route.Name == name {
							container = route.Container
							break
						}
					}
				}
			}
			if row, err := r.q.GetContainer(ctx, store.GetContainerParams{ProjectID: project, Name: container}); err == nil && row.NodeID == node {
				v.Alert.Project = project
			}
		}
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
		latest[a.Key+"\x00"+a.Occurrence] = a.Revision
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
		l.Baselines, l.BaselineAt = nil, 0
		if s.GetBaselinesSupported() {
			l.BaselineAt = time.Now().Unix()
		}
		for _, b := range s.GetBaselines() {
			if raw, err := protojson.Marshal(b); err == nil && len(l.Baselines) < maxBaselines {
				l.Baselines = append(l.Baselines, raw)
			}
		}
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
	if err := r.saveAlertLedger(ctx, node, l, logstore.Event{Kind: "alert.snapshot", Object: node, Data: map[string]any{"source": l.Source, "revision": s.Revision, "current": len(l.Current), "history": len(l.History)}}); err != nil {
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
		ids = []string{""}
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

func (r *Registry) HandleAlert(ctx context.Context, req *nagiv1.HandleAlertRequest, actor string) (*nagiv1.AlertState, error) {
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
		case "unacknowledge":
			a.Acknowledged = false
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
		case "end":
			// One-off panel security events have no future health probe. Ending
			// those records is not recovery; continuous detections cannot be hidden.
			if req.NodeId != "" || a.Alert.Source != "panel" || a.Alert.Status != "firing" || strings.HasPrefix(a.Alert.Key, "offline:") || strings.HasPrefix(a.Alert.Key, "audit-sink:") {
				return nil, status.Error(codes.InvalidArgument, "不支持的处理动作")
			}
			a.Alert.Status, a.Alert.EndReason, a.Alert.Time = "ended", "manual", timestamppb.Now()
			for i, v := range l.Current {
				if v == a {
					l.Current = append(l.Current[:i], l.Current[i+1:]...)
					break
				}
			}
			l.History = append(l.History, a)
			if len(l.History) > 200 {
				l.History = l.History[len(l.History)-200:]
			}
		default:
			return nil, status.Error(codes.InvalidArgument, "不支持的处理动作")
		}
		a.HandlingRevision++
		if err := r.saveAlertLedger(ctx, req.NodeId, l, logstore.Event{Kind: "alert.handled", Object: req.NodeId, Project: a.Alert.Project, Data: map[string]any{"actor": actor, "action": req.Action, "alert_key": req.Key, "occurrence": req.Occurrence, "handling_revision": a.HandlingRevision, "conclusion": a.Conclusion, "silence_until": a.SilenceUntil, "maintenance_until": a.MaintenanceUntil}}); err != nil {
			return nil, err
		}
		r.Changes.Bump()
		return proto.CloneOf(a), nil
	}
	return nil, status.Error(codes.NotFound, "告警不存在或已超出历史保留范围")
}
