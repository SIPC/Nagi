package api

import (
	"context"
	"errors"
	"io"

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
			return 0, i18n.New("节点不在线")
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
			return 0, i18n.New("节点没有返回结果")
		case status.Code(err) == codes.Unimplemented:
			return 0, i18n.New("节点版本过旧，升级节点后才能清理快照仓库")
		}
		return 0, err
	})
}
