package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"google.golang.org/protobuf/proto"
)

// RebuildNodeProject 用面板上的配置备份，把离线节点上的项目重建到另一个节点（见 docs/failover.md#用配置备份重建项目）。
// 重建出来的是一个新项目（新 ID），原节点回来时两份互不干扰。
func (s *projectService) RebuildNodeProject(ctx context.Context, req *connect.Request[nagiv1.MigrateNodeProjectRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	srcID, dstID, id := req.Msg.GetNodeId(), req.Msg.GetTargetNodeId(), req.Msg.GetProjectId()
	if srcID == dstID {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("目标节点不能是项目原来所在的节点"))
	}
	if s.Nodes.Online(srcID) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("项目所在的节点在线，请用迁移（不丢数据）"))
	}
	dst, err := s.agent(dstID)
	if err != nil {
		return err
	}
	src, err := s.projectBackup(ctx, srcID, id)
	if err != nil {
		return err
	}
	_ = out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: fmt.Sprintf(
		"用面板上的配置备份在 %s 上重建 %s（原来在 %s）", s.nodeName(ctx, dstID), src.GetName(), s.nodeName(ctx, srcID))}})
	// 一旦开始就让节点做完，浏览器断开也不中断。
	ctx = context.WithoutCancel(ctx)
	stream, err := dst.RebuildProject(ctx, &nagiv1.RebuildProjectRequest{Project: src, Dir: req.Msg.GetDir()})
	if err != nil {
		return fromGRPC(err)
	}
	return forwardEvents(ctx, stream.Recv, out)
}

// projectBackup 读取并解密面板上某个节点的项目配置备份。
func (s *projectService) projectBackup(ctx context.Context, nodeID, projectID string) (*nagiv1.Project, error) {
	backups, err := s.Q.ListProjectBackups(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, b := range backups {
		if b.ProjectID != projectID {
			continue
		}
		plain, err := s.Sealer.Open(b.Data, nodes.BackupContext(b.NodeID, b.ProjectID))
		if err != nil {
			return nil, connect.NewError(connect.CodeDataLoss, fmt.Errorf("无法解密配置备份：%w", err))
		}
		p := &nagiv1.Project{}
		if err := proto.Unmarshal(plain, p); err != nil {
			return nil, connect.NewError(connect.CodeDataLoss, fmt.Errorf("配置备份损坏：%w", err))
		}
		return p, nil
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("面板上没有这个项目的配置备份"))
}
