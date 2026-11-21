package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc"
)

type nodeLogService struct{ *API }

func (s *nodeLogService) QueryNodeLogs(ctx context.Context, req *connect.Request[nagiv1.NodeLogQuery]) (*connect.Response[nagiv1.LogPage], error) {
	if !auth.CurrentUser(ctx).IsAdmin() {
		return nil, permissionDenied()
	}
	if req.Msg.GetNodeId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请选择节点"))
	}
	cc, err := s.logConnection(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
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
