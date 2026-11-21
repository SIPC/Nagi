package nodes

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Local primary detections share the same durable handling ledger as node alerts.
// Standby-local connectivity warnings remain local audit events, not competing
// writes to the primary's ledger while it is partitioned.
func (r *Registry) trackPanelAlert(ctx context.Context, a *nagiv1.Alert) (bool, error) {
	if r.ReadOnly || r.AlertLogs == nil || a.GetStatus() == "info" {
		return true, nil
	}
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	l, err := r.loadAlertLedger(ctx, "")
	if err != nil {
		return false, err
	}
	l.Source = "panel"
	var state *nagiv1.AlertState
	index := -1
	for i, v := range l.Current {
		if v.Alert.Key == a.Key {
			state, index = v, i
			break
		}
	}
	if state == nil {
		if a.Status != "firing" {
			return false, nil
		}
		if len(l.Current) >= 200 {
			return false, logstore.ErrCapacity
		}
		state = &nagiv1.AlertState{Alert: &nagiv1.Alert{Occurrence: rand.Text(), Since: timestamppb.Now()}}
		l.Current = append(l.Current, state)
	}
	v := proto.CloneOf(a)
	v.Source, v.Occurrence, v.Since, v.Revision, v.Validity = "panel", state.Alert.Occurrence, state.Alert.Since, l.Revision+1, "valid"
	if v.Time == nil {
		v.Time = timestamppb.Now()
	}
	if v.Status == "resolved" {
		v.EndReason = "recovered"
	}
	if v.Status == "ended" && v.EndReason == "" {
		v.EndReason = "rule_disabled"
	}
	state.Alert, state.ReceivedAt = v, timestamppb.Now()
	if index >= 0 && v.Status != "firing" {
		l.Current = append(l.Current[:index], l.Current[index+1:]...)
		l.History = append(l.History, state)
		if len(l.History) > 200 {
			l.History = l.History[len(l.History)-200:]
		}
	}
	if err := r.saveAlertLedger(ctx, "", l, logstore.Event{Kind: "alert.panel_state", Data: map[string]any{"alert_key": v.Key, "occurrence": v.Occurrence, "status": v.Status, "title": v.Title, "message": v.Message, "end_reason": v.EndReason}}); err != nil {
		return false, err
	}
	r.Changes.Bump()
	return v.Status != "firing" || !(state.SilenceUntil.AsTime().After(time.Now()) || state.MaintenanceUntil.AsTime().After(time.Now())), nil
}
