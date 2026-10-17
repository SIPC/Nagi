package api

import (
	"context"
	"errors"
	"fmt"
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

// ---- 手动迁移 ----

// deployStream 是节点返回 DeployEvent 流的调用。
type deployStream interface {
	Recv() (*nagiv1.DeployEvent, error)
}

// forward 把节点的输出转给浏览器，返回最终结果。
func forward(stream deployStream, err error, out *connect.ServerStream[nagiv1.DeployEvent]) *nagiv1.DeployResult {
	if err != nil {
		return &nagiv1.DeployResult{Error: fromGRPC(err).Error()}
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return &nagiv1.DeployResult{Error: "节点没有返回结果"}
		}
		if err != nil {
			return &nagiv1.DeployResult{Error: connectMessage(fromGRPC(err))}
		}
		if r := ev.GetResult(); r != nil {
			return r
		}
		_ = out.Send(ev)
	}
}

func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// MigrateNodeProject 把项目从 A 迁移到 C：C 登记副本 → A 同步、停止、最后一次增量同步 → C 恢复并启动 →
// A 移出项目（目录保留）。任何一步失败都撤销：C 上删掉恢复了一半的项目，A 重新启动项目。
func (s *projectService) MigrateNodeProject(ctx context.Context, req *connect.Request[nagiv1.MigrateNodeProjectRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	srcID, dstID, id := req.Msg.GetNodeId(), req.Msg.GetTargetNodeId(), req.Msg.GetProjectId()
	if srcID == dstID {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("目标节点不能是项目所在的节点"))
	}
	src, err := s.agent(srcID)
	if err != nil {
		return err
	}
	dst, err := s.agent(dstID)
	if err != nil {
		return err
	}
	srcKey, err := s.Nodes.NodeMeshKey(srcID)
	if err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	dstKey, err := s.Nodes.NodeMeshKey(dstID)
	if err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	proj, err := src.GetProject(ctx, &nagiv1.GetProjectRequest{Id: id})
	if err != nil {
		return fromGRPC(err)
	}
	name := proj.GetProject().GetProject().GetName()
	srcName, dstName := s.nodeName(ctx, srcID), s.nodeName(ctx, dstID)
	line := func(l string) { _ = out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: l}}) }
	finish := func(r *nagiv1.DeployResult) error {
		return out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: r}})
	}
	fwd := func(st deployStream, err error) *nagiv1.DeployResult { return forward(st, err, out) }
	// 迁移一旦开始就要走完撤销，浏览器断开也不中断。
	ctx = context.WithoutCancel(ctx)
	resume := func() {
		line("撤销：在 " + srcName + " 上重新启动项目")
		if r := fwd(src.MigrateResume(ctx, &nagiv1.ReplicaRef{ProjectId: id})); !r.GetSuccess() {
			line("重新启动失败：" + r.GetError())
		}
	}

	line(fmt.Sprintf("把 %s 从 %s 迁移到 %s", name, srcName, dstName))
	// 先检查目标节点，有问题当场失败，项目不用停。
	if _, err := dst.CheckRestore(ctx, &nagiv1.CheckRestoreRequest{ProjectId: id, Dir: req.Msg.GetDir()}); err != nil {
		return finish(&nagiv1.DeployResult{Error: "目标节点检查不通过：" + connectMessage(fromGRPC(err))})
	}
	if _, err := dst.AcceptReplica(ctx, &nagiv1.AcceptReplicaRequest{
		ProjectId: id, ProjectName: name, SourceKey: srcKey[:], SourceNodeId: srcID, SourceName: srcName,
	}); err != nil {
		return finish(&nagiv1.DeployResult{Error: connectMessage(fromGRPC(err))})
	}
	if r := fwd(src.MigrateOut(ctx, &nagiv1.MigrateOutRequest{
		ProjectId: id, TargetKey: dstKey[:], TargetNodeId: dstID, TargetName: dstName,
	})); !r.GetSuccess() {
		_, _ = dst.DeleteReplica(ctx, &nagiv1.ReplicaRef{ProjectId: id})
		resume()
		return finish(&nagiv1.DeployResult{Error: "迁移失败：" + r.GetError()})
	}
	line("在 " + dstName + " 上恢复项目")
	if r := fwd(dst.RestoreReplica(ctx, &nagiv1.RestoreReplicaRequest{ProjectId: id, Dir: req.Msg.GetDir()})); !r.GetSuccess() {
		if _, err := dst.GetProject(ctx, &nagiv1.GetProjectRequest{Id: id}); err == nil {
			line("撤销：删除 " + dstName + " 上恢复了一半的项目")
			_, _ = dst.DeleteProject(ctx, &nagiv1.DeleteProjectRequest{Id: id, DeleteDir: true, ConfirmName: name})
		}
		_, _ = dst.DeleteReplica(ctx, &nagiv1.ReplicaRef{ProjectId: id})
		resume()
		return finish(&nagiv1.DeployResult{Error: "迁移失败：" + r.GetError()})
	}
	fin, err := src.MigrateFinish(ctx, &nagiv1.ReplicaRef{ProjectId: id})
	if err != nil {
		line("项目已在 " + dstName + " 上运行，但从 " + srcName + " 移出失败：" + connectMessage(fromGRPC(err)))
	} else {
		line(fmt.Sprintf("已从 %s 移出项目，原目录保留在 %s，确认没问题后可以手动删除", srcName, fin.GetDir()))
	}
	s.Log.Info("项目已迁移", "project", id, "from", srcID, "to", dstID)
	return finish(&nagiv1.DeployResult{Success: true})
}
