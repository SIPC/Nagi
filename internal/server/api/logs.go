package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

type nodeLogService struct{ *API }

func (s *nodeLogService) QueryNodeLogs(ctx context.Context, req *connect.Request[nagiv1.NodeLogQuery]) (*connect.Response[nagiv1.LogPage], error) {
	if !auth.CurrentUser(ctx).IsAdmin() {
		return nil, permissionDenied()
	}
	if s.Panel != nil && s.Panel.Standby != nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("备用面板尚未接入日志 mesh，不能从控制连接读取代替"))
	}
	if req.Msg.GetNodeId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请选择节点"))
	}
	m := s.Nodes.Mesh()
	if m == nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("面板 mesh 没有启动，无法查询节点日志"))
	}
	k, err := s.Nodes.NodeMeshKey(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	cc, err := mesh.Client(m, k)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
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
