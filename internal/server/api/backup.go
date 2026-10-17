package api

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 备份节点：面板只负责登记（两边节点都必须是本面板的在线节点），同步和恢复由节点经 mesh 完成。

func (s *projectService) nodeName(ctx context.Context, id string) string {
	if n, err := s.Q.GetNode(ctx, id); err == nil && n.Name != "" {
		return n.Name
	}
	return id
}

func (s *projectService) SetNodeProjectBackup(ctx context.Context, req *connect.Request[nagiv1.SetNodeProjectBackupRequest]) (*connect.Response[nagiv1.BackupStatus], error) {
	src, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	target := req.Msg.GetBackupNodeId()
	if target == "" {
		// 取消：源节点不再同步，备份节点上已有的副本保留，需要时手动删除。
		st, err := src.SetBackupTarget(ctx, &nagiv1.SetBackupTargetRequest{ProjectId: req.Msg.GetProjectId()})
		if err != nil {
			return nil, fromGRPC(err)
		}
		return connect.NewResponse(st), nil
	}
	if target == req.Msg.GetNodeId() {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("备份节点不能是项目所在的节点"))
	}
	dst, err := s.agent(target)
	if err != nil {
		return nil, err
	}
	srcKey, err := s.Nodes.NodeMeshKey(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	dstKey, err := s.Nodes.NodeMeshKey(target)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	proj, err := src.GetProject(ctx, &nagiv1.GetProjectRequest{Id: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	// 先让备份节点登记源节点，再让源节点开始同步。
	if _, err := dst.AcceptReplica(ctx, &nagiv1.AcceptReplicaRequest{
		ProjectId: req.Msg.GetProjectId(), ProjectName: proj.GetProject().GetProject().GetName(),
		SourceKey: srcKey[:], SourceNodeId: req.Msg.GetNodeId(), SourceName: s.nodeName(ctx, req.Msg.GetNodeId()),
	}); err != nil {
		return nil, fromGRPC(err)
	}
	st, err := src.SetBackupTarget(ctx, &nagiv1.SetBackupTargetRequest{
		ProjectId: req.Msg.GetProjectId(), TargetKey: dstKey[:], TargetNodeId: target, TargetName: s.nodeName(ctx, target),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}

func (s *projectService) GetNodeProjectBackup(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.BackupStatus], error) {
	src, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	st, err := src.GetBackupStatus(ctx, &nagiv1.GetBackupStatusRequest{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}

func (s *projectService) SyncNodeProjectBackup(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.BackupStatus], error) {
	src, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	st, err := src.SyncBackup(ctx, &nagiv1.GetBackupStatusRequest{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}

// ---- 备份节点上的副本 ----

func (s *nodeService) ListNodeReplicas(ctx context.Context, req *connect.Request[nagiv1.ListNodeReplicasRequest]) (*connect.Response[nagiv1.ListReplicasResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListReplicas(ctx, &nagiv1.ListReplicasRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) ListNodeReplicaSnapshots(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.ListSnapshotsResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListReplicaSnapshots(ctx, &nagiv1.ReplicaRef{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) RestoreNodeReplica(ctx context.Context, req *connect.Request[nagiv1.RestoreNodeReplicaRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.RestoreReplica(ctx, &nagiv1.RestoreReplicaRequest{
		ProjectId: req.Msg.GetProjectId(), Dir: req.Msg.GetDir(), SnapshotId: req.Msg.GetSnapshotId(),
	})
	if err != nil {
		return fromGRPC(err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fromGRPC(err)
		}
		if err := out.Send(ev); err != nil {
			return err
		}
	}
}

func (s *nodeService) DeleteNodeReplica(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.DeleteReplicaResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.DeleteReplica(ctx, &nagiv1.ReplicaRef{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}
