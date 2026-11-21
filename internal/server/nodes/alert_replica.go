package nodes

import (
	"context"
	"encoding/json"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *panelMesh) ReadAlertReplica(ctx context.Context, q *nagiv1.AlertReplicaRequest) (*nagiv1.AlertReplica, error) {
	if err := p.authorizeLogPanel(ctx); err != nil {
		return nil, err
	}
	if p.r.ReadOnly {
		return nil, status.Error(codes.FailedPrecondition, "告警处理需要连接当前主面板")
	}
	return p.r.AlertReplica(ctx, q.GetNodeId())
}

func (p *panelMesh) RelayHandleAlert(ctx context.Context, q *nagiv1.RelayHandleAlertRequest) (*nagiv1.AlertReplica, error) {
	if err := p.authorizeLogPanel(ctx); err != nil {
		return nil, err
	}
	if p.r.ReadOnly {
		return nil, status.Error(codes.FailedPrecondition, "告警处理需要连接当前主面板")
	}
	if q.GetRequest() == nil || len(q.GetActor()) > 100 {
		return nil, status.Error(codes.InvalidArgument, "告警状态超出限制")
	}
	caller, _ := mesh.Caller(ctx, p.M) // authenticated above; bind the claimed actor to its panel
	if _, err := p.r.HandleAlert(ctx, q.Request, caller.String()+":"+q.Actor); err != nil {
		return nil, err
	}
	return p.r.AlertReplica(ctx, q.Request.NodeId)
}

func (r *Registry) AlertReplica(ctx context.Context, node string) (*nagiv1.AlertReplica, error) {
	if r.AlertLogs == nil {
		return nil, status.Error(codes.Unavailable, "告警日志库没有启动")
	}
	b, err := r.AlertLogs.Checkpoint(ctx, alertStateKey(node))
	return &nagiv1.AlertReplica{NodeId: node, LedgerJson: b}, err
}

// Only called with a response from the certificate-bound current primary. The
// whole ledger replaces detection and handling atomically; report ingestion uses
// the same mutex. A standby never accepts writes independently of its primary.
func (r *Registry) ImportAlertReplica(ctx context.Context, v *nagiv1.AlertReplica) error {
	if !r.ReadOnly || v == nil || len(v.LedgerJson) == 0 {
		return nil
	}
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	old, err := r.AlertLogs.Checkpoint(ctx, alertStateKey(v.NodeId))
	if err != nil {
		return err
	}
	if string(old) == string(v.LedgerJson) {
		return nil
	}
	var before, incoming alertLedger
	if err := json.Unmarshal(v.LedgerJson, &incoming); err != nil {
		return err
	}
	if len(old) != 0 {
		if err := json.Unmarshal(old, &before); err != nil {
			return err
		}
		if incoming.Term < before.Term || incoming.Term == before.Term && incoming.Revision <= before.Revision {
			return nil
		}
	}
	return r.AlertLogs.SaveAlertLedger(ctx, v.NodeId, v.LedgerJson, logstore.Event{Kind: "alert.replica", Object: v.NodeId, Data: map[string]any{"primary_copy": true}})
}

func (r *Registry) RefreshAlertReplica(ctx context.Context, node string) error {
	if r.AlertPrimary == nil {
		return status.Error(codes.Unavailable, "告警处理需要连接当前主面板")
	}
	cc, err := r.AlertPrimary(ctx)
	if err != nil {
		return err
	}
	defer cc.Close()
	v, err := nagiv1.NewPanelAlertServiceClient(cc).ReadAlertReplica(ctx, &nagiv1.AlertReplicaRequest{NodeId: node})
	if err != nil {
		return err
	}
	return r.ImportAlertReplica(ctx, v)
}

func (r *Registry) RunAlertReplica(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		list, err := r.q.ListNodes(ctx)
		if err == nil {
			ids := []string{""} // panel-generated events
			for _, n := range list {
				ids = append(ids, n.ID)
			}
			for _, id := range ids {
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := r.RefreshAlertReplica(callCtx, id)
				cancel()
				if err != nil {
					break
				} // retry once per cycle, not once per node
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
