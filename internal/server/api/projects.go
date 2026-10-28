package api

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"google.golang.org/grpc/status"
)

// projectService 是浏览器调用的项目接口，实现在 projects 包里。
type projectService struct{ *API }

// toConnect 把项目操作的错误转换成 Connect 错误码。
func toConnect(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, spec.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, projects.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, projects.ErrConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, nodes.ErrOffline):
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, nodes.ErrPending):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if _, ok := status.FromError(err); ok {
		return fromGRPC(err)
	}
	return err
}

func reply[T any](m *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(m), nil
}

// streamOp 执行一个会输出过程的操作，把输出和结果发给浏览器。参数错误、冲突这些直接作为错误返回。
// 结果里的错误按 ctx 里的语言显示（节点直接返回的错误除外）。
func streamOp(ctx context.Context, out *connect.ServerStream[nagiv1.DeployEvent], op func(projects.Output) (int64, error)) error {
	version, err := op(func(line string) {
		_ = out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	if err != nil && (errors.Is(err, spec.ErrInvalid) || errors.Is(err, projects.ErrNotFound) || errors.Is(err, projects.ErrConflict)) {
		return toConnect(err)
	}
	res := &nagiv1.DeployResult{Success: err == nil, Version: version}
	if err != nil {
		if isStatus(err) {
			res.Error = status.Convert(err).Message()
		} else {
			res.Error = i18n.Localize(i18n.FromContext(ctx), err)
		}
	}
	return out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: res}})
}

func (s *projectService) ListProjects(ctx context.Context, _ *connect.Request[nagiv1.ListProjectsRequest]) (*connect.Response[nagiv1.ListProjectsResponse], error) {
	list, err := s.Projects.List(ctx)
	return reply(&nagiv1.ListProjectsResponse{Projects: list}, err)
}

func (s *projectService) CreateProject(ctx context.Context, req *connect.Request[nagiv1.CreateProjectRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	cr := projects.CreateRequest{Name: m.GetName(), NetworkName: m.GetNetworkName(), Settings: m.GetSettings()}
	if m.GetContainer() != nil {
		cr.Container = &projects.AddRequest{Name: m.GetContainerName(), Config: m.GetContainer(), NodeID: m.GetNodeId(), Dir: m.GetDir()}
	}
	p, err := s.Projects.Create(ctx, cr)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) GetProject(ctx context.Context, req *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.ProjectSummary], error) {
	p, err := s.Projects.Get(ctx, req.Msg.GetProjectId())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) UpdateProjectSettings(ctx context.Context, req *connect.Request[nagiv1.UpdateProjectSettingsRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	p, err := s.Projects.UpdateSettings(ctx, m.GetProjectId(), m.GetBaseRevision(), m.GetName(), m.GetNetworkName(), m.GetSettings())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) SaveSharedEnv(ctx context.Context, req *connect.Request[nagiv1.SaveSharedEnvRequest]) (*connect.Response[nagiv1.SaveSharedEnvResponse], error) {
	p, affected, err := s.Projects.SaveSharedEnv(ctx, req.Msg.GetProjectId(), req.Msg.GetBaseRevision(), req.Msg.GetVars())
	return reply(&nagiv1.SaveSharedEnvResponse{Project: p, Affected: affected}, err)
}

func (s *projectService) DiscardSharedEnv(ctx context.Context, req *connect.Request[nagiv1.ProjectRevisionRef]) (*connect.Response[nagiv1.Project], error) {
	p, err := s.Projects.DiscardSharedEnv(ctx, req.Msg.GetProjectId(), req.Msg.GetBaseRevision())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) DeleteProject(ctx context.Context, req *connect.Request[nagiv1.DeleteProjectRequest]) (*connect.Response[nagiv1.DeleteProjectResponse], error) {
	m := req.Msg
	err := s.Projects.Delete(ctx, m.GetProjectId(), m.GetBaseRevision(), m.GetDeleteDir(), m.GetConfirmName())
	return reply(&nagiv1.DeleteProjectResponse{}, err)
}

func (s *projectService) AddContainer(ctx context.Context, req *connect.Request[nagiv1.AddContainerRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	p, err := s.Projects.AddContainer(ctx, projects.AddRequest{ProjectID: m.GetProjectId(), Name: m.GetName(), Config: m.GetConfig(), NodeID: m.GetNodeId(), Dir: m.GetDir()})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) DeleteContainer(ctx context.Context, req *connect.Request[nagiv1.DeleteContainerApiRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	p, err := s.Projects.DeleteContainer(ctx, m.GetProjectId(), m.GetContainer(), m.GetBaseRevision(), m.GetDeleteDir())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) SaveDraft(ctx context.Context, req *connect.Request[nagiv1.SaveDraftRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	p, err := s.Projects.SaveDraft(ctx, m.GetProjectId(), m.GetContainer(), m.GetBaseRevision(), m.GetConfig())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) DiscardDraft(ctx context.Context, req *connect.Request[nagiv1.ContainerRevisionRef]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	p, err := s.Projects.DiscardDraft(ctx, m.GetProjectId(), m.GetContainer(), m.GetBaseRevision())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) Deploy(ctx context.Context, req *connect.Request[nagiv1.DeployRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	targets := make([]projects.Target, 0, len(m.GetTargets()))
	for _, t := range m.GetTargets() {
		targets = append(targets, projects.Target{Container: t.GetContainer(), BaseRevision: t.GetBaseRevision()})
	}
	o := projects.DeployOptions{
		Pull: m.GetPull(), Upload: m.GetUseUpload(), PullImages: m.GetPullImages(),
		ApplySharedEnv: m.GetApplySharedEnv(), SettingsRevision: m.GetSettingsRevision(),
	}
	return streamOp(ctx, out, func(o2 projects.Output) (int64, error) {
		return s.Projects.Deploy(ctx, m.GetProjectId(), targets, o, o2)
	})
}

func (s *projectService) ListVersions(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.ListVersionsResponse], error) {
	list, err := s.Projects.Versions(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	return reply(&nagiv1.ListVersionsResponse{Versions: list}, err)
}

func (s *projectService) Rollback(ctx context.Context, req *connect.Request[nagiv1.RollbackRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return s.Projects.Rollback(ctx, m.GetProjectId(), m.GetContainer(), m.GetVersion(), m.GetRestoreData(), o)
	})
}

func (s *projectService) ListDeployments(ctx context.Context, req *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.ListDeploymentsResponse], error) {
	list, err := s.Projects.Deployments(ctx, req.Msg.GetProjectId())
	return reply(&nagiv1.ListDeploymentsResponse{Deployments: list}, err)
}

func (s *projectService) RestoreDeployment(ctx context.Context, req *connect.Request[nagiv1.RestoreDeploymentRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return s.Projects.RestoreDeployment(ctx, m.GetProjectId(), m.GetDeploymentId(), o)
	})
}

func (s *projectService) StreamLogs(ctx context.Context, req *connect.Request[nagiv1.StreamLogsApiRequest], out *connect.ServerStream[nagiv1.LogLine]) error {
	m := req.Msg
	stream, err := s.Projects.Logs(ctx, m.GetProjectId(), m.GetContainer(), m.GetService(), m.GetTail(), m.GetFollow())
	if err != nil {
		return toConnect(err)
	}
	for {
		line, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fromGRPC(err)
		}
		if err := out.Send(line); err != nil {
			return err
		}
	}
}

func (s *projectService) SaveRoute(ctx context.Context, req *connect.Request[nagiv1.SaveRouteRequest]) (*connect.Response[nagiv1.Project], error) {
	p, err := s.Projects.SaveRoute(ctx, req.Msg.GetProjectId(), req.Msg.GetRoute())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) DeleteRoute(ctx context.Context, req *connect.Request[nagiv1.DeleteRouteRequest]) (*connect.Response[nagiv1.Project], error) {
	p, err := s.Projects.DeleteRoute(ctx, req.Msg.GetProjectId(), req.Msg.GetName())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(p), nil
}

func (s *projectService) SetBackupNode(ctx context.Context, req *connect.Request[nagiv1.SetBackupNodeRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	return reply(s.Projects.SetBackupNode(ctx, m.GetProjectId(), m.GetContainer(), m.GetNodeId(), m.GetDeleteReplica()))
}

func (s *projectService) SyncBackup(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.SyncBackup(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer()))
}

func (s *projectService) ListBackupSnapshots(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.ListSnapshotsResponse], error) {
	list, err := s.Projects.BackupSnapshots(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	return reply(&nagiv1.ListSnapshotsResponse{Snapshots: list}, err)
}

func (s *projectService) RestoreFromBackup(ctx context.Context, req *connect.Request[nagiv1.RestoreFromBackupRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return 0, s.Projects.RestoreFromBackup(ctx, m.GetProjectId(), m.GetContainer(), m.GetSnapshotId(), m.GetDir(), o)
	})
}

func (s *projectService) MigrateContainer(ctx context.Context, req *connect.Request[nagiv1.MoveContainerRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return 0, s.Projects.Migrate(ctx, m.GetProjectId(), m.GetContainer(), m.GetNodeId(), m.GetDir(), o)
	})
}

func (s *projectService) RedeployElsewhere(ctx context.Context, req *connect.Request[nagiv1.MoveContainerRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return 0, s.Projects.RedeployElsewhere(ctx, m.GetProjectId(), m.GetContainer(), m.GetNodeId(), m.GetDir(), o)
	})
}

func (s *projectService) SetFailover(ctx context.Context, req *connect.Request[nagiv1.SetFailoverApiRequest]) (*connect.Response[nagiv1.Project], error) {
	m := req.Msg
	return reply(s.Projects.SetFailover(ctx, projects.SetFailoverRequest{
		ProjectID: m.GetProjectId(), Container: m.GetContainer(), StandbyNodeID: m.GetStandbyNodeId(), StandbyDir: m.GetStandbyDir(),
		WitnessNodeID: m.GetWitnessNodeId(), LeaseSeconds: m.GetLeaseSeconds(), AutoStateful: m.GetAutoStateful(), AutoFailback: m.GetAutoFailback(),
	}))
}

func (s *projectService) ClearFailover(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.ClearFailover(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer()))
}

func (s *projectService) ChangeWitness(ctx context.Context, req *connect.Request[nagiv1.ChangeWitnessRequest]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.ChangeWitness(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer(), req.Msg.GetWitnessNodeId()))
}

func (s *projectService) ConfirmTakeover(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.ConfirmTakeover(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer()))
}

func (s *projectService) ConnectProject(ctx context.Context, req *connect.Request[nagiv1.ConnectProjectRequest]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.Connect(ctx, req.Msg.GetProjectId(), req.Msg.GetOtherProjectId()))
}

func (s *projectService) DisconnectProject(ctx context.Context, req *connect.Request[nagiv1.ConnectProjectRequest]) (*connect.Response[nagiv1.Project], error) {
	return reply(s.Projects.Disconnect(ctx, req.Msg.GetProjectId(), req.Msg.GetOtherProjectId()))
}

func (s *projectService) ListSnapshots(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.ListSnapshotsResponse], error) {
	list, err := s.Projects.Snapshots(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	return reply(&nagiv1.ListSnapshotsResponse{Snapshots: list}, err)
}

func (s *projectService) CreateSnapshot(ctx context.Context, req *connect.Request[nagiv1.ContainerRef], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return 0, s.Projects.CreateSnapshot(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer(), o)
	})
}

func (s *projectService) RestoreSnapshot(ctx context.Context, req *connect.Request[nagiv1.RestoreSnapshotRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	m := req.Msg
	return streamOp(ctx, out, func(o projects.Output) (int64, error) {
		return 0, s.Projects.RestoreSnapshot(ctx, m.GetProjectId(), m.GetContainer(), m.GetSnapshotId(), o)
	})
}

func (s *projectService) GetBackupKey(ctx context.Context, req *connect.Request[nagiv1.ContainerRef]) (*connect.Response[nagiv1.GetBackupKeyResponse], error) {
	key, err := s.Projects.BackupKey(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	return reply(&nagiv1.GetBackupKeyResponse{Key: key}, err)
}

func (s *projectService) ListRegistries(ctx context.Context, req *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.RegistriesResponse], error) {
	list, err := s.Projects.Registries(ctx, req.Msg.GetProjectId())
	return reply(&nagiv1.RegistriesResponse{Registries: list}, err)
}

func (s *projectService) LoginRegistry(ctx context.Context, req *connect.Request[nagiv1.LoginRegistryApiRequest]) (*connect.Response[nagiv1.RegistriesResponse], error) {
	m := req.Msg
	list, err := s.Projects.LoginRegistry(ctx, m.GetProjectId(), m.GetRegistry(), m.GetUsername(), m.GetPassword())
	return reply(&nagiv1.RegistriesResponse{Registries: list}, err)
}

func (s *projectService) LogoutRegistry(ctx context.Context, req *connect.Request[nagiv1.LogoutRegistryRequest]) (*connect.Response[nagiv1.RegistriesResponse], error) {
	list, err := s.Projects.LogoutRegistry(ctx, req.Msg.GetProjectId(), req.Msg.GetRegistry())
	return reply(&nagiv1.RegistriesResponse{Registries: list}, err)
}

func (s *projectService) CheckImages(ctx context.Context, req *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.CheckImagesResponse], error) {
	list, err := s.Projects.CheckImages(ctx, req.Msg.GetProjectId())
	return reply(&nagiv1.CheckImagesResponse{Updates: list}, err)
}
