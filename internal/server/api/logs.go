package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nodeLogService struct{ *API }

func (s *nodeLogService) QueryNodeLogs(ctx context.Context, req *connect.Request[nagiv1.NodeLogQuery]) (*connect.Response[nagiv1.LogPage], error) {
	if !auth.CurrentUser(ctx).IsAdmin() {
		return nil, permissionDenied()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if req.Msg.GetPanelLocal() {
		if req.Msg.GetNodeId() != "" || req.Msg.GetRecentCopy() {
			return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请选择节点"))
		}
		return s.storedLogs(ctx, req.Msg)
	}
	if req.Msg.GetNodeId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请选择节点"))
	}
	if req.Msg.GetRecentCopy() {
		return s.storedLogs(ctx, req.Msg)
	}
	cc, err := s.logConnection(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	q := req.Msg.GetQuery()
	if q == nil {
		q = &nagiv1.LogQuery{}
	}
	out, err := nagiv1.NewLogServiceClient(cc).QueryLogs(ctx, q)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeLogService) storedLogs(ctx context.Context, req *nagiv1.NodeLogQuery) (*connect.Response[nagiv1.LogPage], error) {
	if req.GetPanelLocal() && s.Logs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("本机日志库没有启动"))
	}
	if req.GetRecentCopy() && s.Nodes.LogCopies == nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("面板近期副本没有启动"))
	}
	q := req.GetQuery()
	if q.GetAfter() > 0 && q.GetSource() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("无效的日志游标或分页大小"))
	}
	f := logstore.Filter{After: q.GetAfter(), Limit: int(q.GetLimit()), Category: q.GetCategory(), Project: q.GetProject(), Object: q.GetObject(), Search: q.GetSearch(), KindPrefix: q.GetKindPrefix()}
	for _, v := range []struct {
		in  *timestamppb.Timestamp
		out *time.Time
	}{{q.GetFrom(), &f.From}, {q.GetTo(), &f.To}} {
		if v.in != nil {
			if v.in.CheckValid() != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("无效的日志时间"))
			}
			t := v.in.AsTime()
			if !time.Unix(0, t.UnixNano()).Equal(t) {
				return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("日志时间超出支持范围"))
			}
			*v.out = t
		}
	}
	var p logstore.Page
	var sourcePruned []logstore.Pruned
	var err error
	if req.GetPanelLocal() {
		p, err = s.Logs.ReadPage(ctx, f)
		if q.GetSource() != "" && p.State.Source != "" && p.State.Source != q.GetSource() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("日志来源已变化，请从头查询；不能沿用旧游标"))
		}
	} else {
		p, sourcePruned, err = s.Nodes.LogCopies.Read(ctx, req.GetNodeId(), q.GetSource(), f)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("尚未收到该节点此类别的近期副本"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	out := &nagiv1.LogPage{Source: p.State.Source, LatestSeq: p.State.Last, NextAfter: p.Next, More: p.More, RecentCopy: req.GetRecentCopy(), WriteErrors: p.State.WriteErrors, DiskBytes: p.State.DiskBytes, BudgetBytes: p.State.BudgetBytes}
	for _, e := range p.Events {
		data, err := json.Marshal(e.Data)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Entries = append(out.Entries, &nagiv1.LogEntry{Seq: e.Seq, Time: timestamppb.New(e.Time), Received: timestamppb.New(e.Received), Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, DataJson: data})
	}
	for _, p := range p.State.Pruned {
		out.Pruned = append(out.Pruned, &nagiv1.LogPruned{Category: p.Category, First: p.First, Last: p.Last, Count: p.Count})
	}
	for _, p := range sourcePruned {
		out.SourcePruned = append(out.SourcePruned, &nagiv1.LogPruned{Category: p.Category, First: p.First, Last: p.Last, Count: p.Count})
	}
	return connect.NewResponse(out), nil
}

// Log and metric history never fall back to the control connection, including
// on standby panels whose mesh membership is not implemented yet.
func (a *API) logConnection(nodeID string) (*grpc.ClientConn, error) {
	if a.Panel != nil && a.Panel.Standby != nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("备用面板尚未接入日志 mesh，不能从控制连接读取代替"))
	}
	m := a.Nodes.Mesh()
	if m == nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("面板 mesh 没有启动，无法查询节点日志"))
	}
	k, err := a.Nodes.NodeMeshKey(nodeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	cc, err := mesh.Client(m, k)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return cc, nil
}

func (a *API) queryNodeMetrics(ctx context.Context, nodeID string, q *nagiv1.QueryMetricsRequest) (*nagiv1.QueryMetricsResponse, error) {
	cc, err := a.logConnection(nodeID)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := nagiv1.NewLogServiceClient(cc).QueryMetrics(ctx, q)
	if err != nil {
		return nil, toConnect(err)
	}
	return out, nil
}
