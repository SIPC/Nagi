package api

import (
	"context"
	"errors"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/projects"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReclaimNodeSnapshots 让节点立即清理它上面每个容器的快照仓库（节点设置里的「清理快照仓库」，见 docs/snapshot.md#立即清理），
// 把节点的输出转给浏览器。NodeService 的修改接口只有管理员能用；开始了就做完，浏览器断开也不中断。
func (s *nodeService) ReclaimNodeSnapshots(ctx context.Context, req *connect.Request[nagiv1.NodeRef], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		conn, err := s.Nodes.Get(req.Msg.GetNodeId())
		if err != nil {
			return 0, err
		}
		stream, err := conn.Agent.ReclaimSnapshots(context.WithoutCancel(ctx), &nagiv1.ReclaimSnapshotsRequest{})
		for err == nil {
			var ev *nagiv1.DeployEvent
			if ev, err = stream.Recv(); err != nil {
				break
			}
			if line := ev.GetOutput(); line != "" {
				o(line)
			}
			if r := ev.GetResult(); r != nil {
				if !r.GetSuccess() {
					return 0, errors.New(r.GetError())
				}
				return 0, nil
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return 0, i18n.New("节点没有返回结果，请升级节点后重试")
		case status.Code(err) == codes.Unimplemented:
			return 0, i18n.Errorf("节点 %s 版本过低，升级后才能清理快照仓库", s.nodeName(ctx, req.Msg.GetNodeId()))
		}
		return 0, err
	})
}

// SetNodeReplicaDir 修改节点保存副本的默认目录，节点把默认目录里的副本移过去（见 docs/snapshot.md#副本的存放位置），
// 把节点的输出转给浏览器。只有管理员能用（NodeService 的修改接口），要先做高权限确认（elevationRequired）：
// 会在节点上任意位置建目录、写数据。路径由节点校验。开始了就做完，浏览器断开也不中断。
func (s *nodeService) SetNodeReplicaDir(ctx context.Context, req *connect.Request[nagiv1.SetNodeReplicaDirRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		nodeID := req.Msg.GetNodeId()
		conn, err := s.Nodes.Get(nodeID)
		if err != nil {
			return 0, err
		}
		if !conn.LastReport().GetNode().GetReplicaDirs() {
			return 0, i18n.Errorf("节点 %s 版本过低，升级后才能选择副本的存放位置", s.nodeName(ctx, nodeID))
		}
		stream, err := conn.Agent.SetReplicaDir(context.WithoutCancel(ctx), &nagiv1.SetReplicaDirRequest{Dir: strings.TrimSpace(req.Msg.GetDir())})
		for err == nil {
			var ev *nagiv1.DeployEvent
			if ev, err = stream.Recv(); err != nil {
				break
			}
			if line := ev.GetOutput(); line != "" {
				o(line)
			}
			if r := ev.GetResult(); r != nil {
				if !r.GetSuccess() {
					return 0, errors.New(r.GetError())
				}
				return 0, nil
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return 0, i18n.New("节点没有返回结果，请升级节点后重试")
		case status.Code(err) == codes.Unimplemented:
			return 0, i18n.Errorf("节点 %s 版本过低，升级后才能选择副本的存放位置", s.nodeName(ctx, nodeID))
		}
		return 0, err
	})
}
