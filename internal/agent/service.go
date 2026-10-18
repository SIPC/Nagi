package agent

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"time"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// agentService 实现 AgentService，供面板通过反向 gRPC 调用。
type agentService struct {
	nagiv1.UnimplementedAgentServiceServer
	node     *agent
	projects *project.Manager
	metrics  *metrics.Collector
	alerts   *alerts.Engine
	panel    string // 当前连接的面板指纹
	changed  func()
}

func (s *agentService) SetAlertConfig(_ context.Context, req *nagiv1.SetAlertConfigRequest) (*nagiv1.SetAlertConfigResponse, error) {
	if err := s.alerts.SetConfig(s.panel, req.GetConfig()); err != nil {
		return nil, status.Errorf(codes.Internal, "保存告警配置: %v", err)
	}
	return &nagiv1.SetAlertConfigResponse{}, nil
}

func (s *agentService) QueryMetrics(ctx context.Context, req *nagiv1.QueryMetricsRequest) (*nagiv1.QueryMetricsResponse, error) {
	to := time.Now()
	if req.GetTo() != nil {
		to = req.GetTo().AsTime()
	}
	from := to.Add(-time.Hour)
	if req.GetFrom() != nil {
		from = req.GetFrom().AsTime()
	}
	data, err := s.metrics.Query(ctx, req.GetPrefix(), from, to)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &nagiv1.QueryMetricsResponse{}
	for name, points := range data {
		ser := &nagiv1.MetricSeries{Name: name}
		for _, p := range points {
			ser.Times = append(ser.Times, p.Time.Unix())
			ser.Values = append(ser.Values, p.Value)
		}
		out.Series = append(out.Series, ser)
	}
	sort.Slice(out.Series, func(i, j int) bool { return out.Series[i].Name < out.Series[j].Name })
	return out, nil
}

func (s *agentService) ListProjects(ctx context.Context, _ *nagiv1.ListProjectsRequest) (*nagiv1.ListProjectsResponse, error) {
	list, err := s.projects.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListProjectsResponse{Projects: list}, nil
}

func (s *agentService) CreateProject(ctx context.Context, req *nagiv1.CreateProjectRequest) (*nagiv1.CreateProjectResponse, error) {
	p, err := s.projects.Create(ctx, req.GetName(), req.GetDir(), req.GetSettings(), req.GetMemberName(), req.GetMember())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.CreateProjectResponse{Project: p}, nil
}

func (s *agentService) GetProject(ctx context.Context, req *nagiv1.GetProjectRequest) (*nagiv1.GetProjectResponse, error) {
	p, err := s.projects.Get(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.GetProjectResponse{Project: p}, nil
}

func (s *agentService) AddMember(ctx context.Context, req *nagiv1.AddMemberRequest) (*nagiv1.AddMemberResponse, error) {
	p, err := s.projects.AddMember(ctx, req.GetProjectId(), req.GetName(), req.GetConfig())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.AddMemberResponse{Project: p}, nil
}

func (s *agentService) DeleteMember(ctx context.Context, req *nagiv1.DeleteMemberRequest) (*nagiv1.DeleteMemberResponse, error) {
	p, err := s.projects.DeleteMember(ctx, req.GetProjectId(), req.GetMember(), req.GetBaseRevision(), req.GetDeleteDir(), func(string) {})
	s.changed()
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.DeleteMemberResponse{Project: p}, nil
}

func (s *agentService) SaveDraft(ctx context.Context, req *nagiv1.SaveDraftRequest) (*nagiv1.SaveDraftResponse, error) {
	p, err := s.projects.SaveDraft(ctx, req.GetId(), req.GetMember(), req.GetBaseRevision(), req.GetConfig())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.SaveDraftResponse{Project: p}, nil
}

func (s *agentService) DiscardDraft(ctx context.Context, req *nagiv1.DiscardDraftRequest) (*nagiv1.DiscardDraftResponse, error) {
	p, err := s.projects.DiscardDraft(ctx, req.GetId(), req.GetMember(), req.GetBaseRevision())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.DiscardDraftResponse{Project: p}, nil
}

func (s *agentService) UpdateProjectSettings(ctx context.Context, req *nagiv1.UpdateProjectSettingsRequest) (*nagiv1.UpdateProjectSettingsResponse, error) {
	p, err := s.projects.UpdateSettings(ctx, req.GetProjectId(), req.GetBaseRevision(), req.GetName(), req.GetSettings(), func(l string) {
		s.node.log.Info(l, "project", req.GetProjectId())
	})
	s.changed()
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.UpdateProjectSettingsResponse{Project: p}, nil
}

func (s *agentService) SaveSharedEnv(ctx context.Context, req *nagiv1.SaveSharedEnvRequest) (*nagiv1.SaveSharedEnvResponse, error) {
	p, affected, err := s.projects.SaveSharedEnv(ctx, req.GetProjectId(), req.GetBaseRevision(), req.GetVars())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.SaveSharedEnvResponse{Project: p, Affected: affected}, nil
}

func (s *agentService) DiscardSharedEnv(ctx context.Context, req *nagiv1.DiscardSharedEnvRequest) (*nagiv1.DiscardSharedEnvResponse, error) {
	p, err := s.projects.DiscardSharedEnv(ctx, req.GetProjectId(), req.GetBaseRevision())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.DiscardSharedEnvResponse{Project: p}, nil
}

func (s *agentService) ListDeployments(ctx context.Context, req *nagiv1.ListDeploymentsRequest) (*nagiv1.ListDeploymentsResponse, error) {
	list, err := s.projects.Deployments(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListDeploymentsResponse{Deployments: list}, nil
}

func (s *agentService) RestoreDeployment(req *nagiv1.RestoreDeploymentRequest, stream nagiv1.AgentService_RestoreDeploymentServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		return s.projects.RestoreDeployment(stream.Context(), req.GetId(), req.GetDeploymentId(), onLine)
	})
}

func (s *agentService) ListSharedNetworks(ctx context.Context, _ *nagiv1.ListSharedNetworksRequest) (*nagiv1.ListSharedNetworksResponse, error) {
	return s.sharedNetworks(ctx)
}

func (s *agentService) sharedNetworks(ctx context.Context) (*nagiv1.ListSharedNetworksResponse, error) {
	list, err := s.projects.SharedNetworks(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	s.node.links.decorate(list)
	return &nagiv1.ListSharedNetworksResponse{Networks: list}, nil
}

func (s *agentService) SetSharedNetworkPeers(ctx context.Context, req *nagiv1.SetSharedNetworkPeersRequest) (*nagiv1.ListSharedNetworksResponse, error) {
	var self []byte
	if s.node.mesh != nil {
		k := s.node.mesh.m.PublicKey()
		self = k[:]
	}
	var peers []project.LinkPeer
	for _, p := range req.GetPeers() {
		if len(p.GetPublicKey()) != 32 || bytes.Equal(p.GetPublicKey(), self) {
			continue
		}
		peers = append(peers, project.LinkPeer{Key: p.GetPublicKey(), Name: p.GetName(), NodeID: p.GetNodeId()})
	}
	if err := s.projects.SetSharedNetworkPeers(ctx, req.GetName(), peers); err != nil {
		return nil, toStatus(err)
	}
	s.node.links.sync(ctx)
	return s.sharedNetworks(ctx)
}

func (s *agentService) CreateSharedNetwork(ctx context.Context, req *nagiv1.CreateSharedNetworkRequest) (*nagiv1.ListSharedNetworksResponse, error) {
	if _, err := s.projects.CreateSharedNetwork(ctx, req.GetName()); err != nil {
		return nil, toStatus(err)
	}
	return s.sharedNetworks(ctx)
}

func (s *agentService) DeleteSharedNetwork(ctx context.Context, req *nagiv1.DeleteSharedNetworkRequest) (*nagiv1.ListSharedNetworksResponse, error) {
	if _, err := s.projects.DeleteSharedNetwork(ctx, req.GetName()); err != nil {
		return nil, toStatus(err)
	}
	return s.sharedNetworks(ctx)
}

func (s *agentService) SaveRoute(ctx context.Context, req *nagiv1.SaveRouteRequest) (*nagiv1.SaveRouteResponse, error) {
	p, err := s.projects.SaveRoute(ctx, req.GetProjectId(), req.GetRoute())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.SaveRouteResponse{Project: p}, nil
}

func (s *agentService) DeleteRoute(ctx context.Context, req *nagiv1.DeleteRouteRequest) (*nagiv1.DeleteRouteResponse, error) {
	p, err := s.projects.DeleteRoute(ctx, req.GetProjectId(), req.GetName())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.DeleteRouteResponse{Project: p}, nil
}

func (s *agentService) DeleteProject(ctx context.Context, req *nagiv1.DeleteProjectRequest) (*nagiv1.DeleteProjectResponse, error) {
	err := s.projects.Delete(ctx, req.GetId(), req.GetBaseRevision(), req.GetDeleteDir(), req.GetConfirmName(), func(string) {})
	s.changed()
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.DeleteProjectResponse{}, nil
}

func (s *agentService) DeployProject(req *nagiv1.DeployProjectRequest, stream nagiv1.AgentService_DeployProjectServer) error {
	defer s.changed()
	targets := make([]project.Target, 0, len(req.GetTargets()))
	for _, t := range req.GetTargets() {
		targets = append(targets, project.Target{Member: t.GetMember(), BaseRevision: t.GetBaseRevision()})
	}
	opts := project.DeployOptions{
		Pull: req.GetPull(), Upload: req.GetUseUpload(), PullImages: req.GetPullImages(),
		ApplySharedEnv: req.GetApplySharedEnv(), SettingsRevision: req.GetSettingsRevision(), Source: req.GetSource(),
	}
	if opts.Source == "" && opts.ApplySharedEnv {
		opts.Source = "shared_env"
	} else if opts.Source == "" && opts.PullImages {
		opts.Source = "image"
	}
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		return s.projects.Deploy(stream.Context(), req.GetId(), targets, opts, onLine)
	})
}

// RebuildProject 用面板的配置备份在本节点上重建项目。项目建好以后部署失败时，结果里仍带新项目的 ID。
func (s *agentService) RebuildProject(req *nagiv1.RebuildProjectRequest, stream nagiv1.AgentService_RebuildProjectServer) error {
	defer s.changed()
	id, err := s.projects.Rebuild(stream.Context(), req.GetProject(), req.GetDir(), func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	if id == "" && err != nil {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil, ProjectId: id}
	if err != nil {
		result.Error = err.Error()
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) StreamLogs(req *nagiv1.StreamLogsRequest, stream nagiv1.AgentService_StreamLogsServer) error {
	err := s.projects.Logs(stream.Context(), req.GetId(), req.GetMember(), req.GetService(), int(req.GetTail()), req.GetFollow(), func(l docker.LogLine) {
		_ = stream.Send(&nagiv1.LogLine{
			Member: l.Member, Service: l.Service, Stream: l.Stream, Text: l.Text, Time: timestamppb.New(l.Time),
		})
	})
	return toStatus(err)
}

func (s *agentService) Exec(stream nagiv1.AgentService_ExecServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "第一条消息必须是 start")
	}
	size := start.GetSize()
	ex, err := s.projects.Exec(ctx, start.GetProjectId(), start.GetMember(), start.GetService(), start.GetCommand(), uint(size.GetCols()), uint(size.GetRows()))
	if err != nil {
		return toStatus(err)
	}
	defer ex.Close()

	// 输入：面板 → 终端
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				ex.Close()
				return
			}
			switch v := in.Input.(type) {
			case *nagiv1.ExecInput_Stdin:
				if _, err := ex.Write(v.Stdin); err != nil {
					return
				}
			case *nagiv1.ExecInput_Resize:
				_ = ex.Resize(ctx, uint(v.Resize.GetCols()), uint(v.Resize.GetRows()))
			}
		}
	}()

	// 输出：终端 → 面板
	buf := make([]byte, 32*1024)
	for {
		n, err := ex.Read(buf)
		if n > 0 {
			if err := stream.Send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_Stdout{Stdout: append([]byte(nil), buf[:n]...)}}); err != nil {
				return err
			}
		}
		if err != nil {
			break
		}
	}
	code := ex.ExitCode(context.WithoutCancel(ctx))
	return stream.Send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_ExitCode{ExitCode: int32(code)}})
}

func (s *agentService) ListVersions(ctx context.Context, req *nagiv1.ListVersionsRequest) (*nagiv1.ListVersionsResponse, error) {
	list, err := s.projects.Versions(ctx, req.GetId(), req.GetMember())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListVersionsResponse{Versions: list}, nil
}

func (s *agentService) ListSnapshots(ctx context.Context, req *nagiv1.ListSnapshotsRequest) (*nagiv1.ListSnapshotsResponse, error) {
	list, err := s.projects.Snapshots(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListSnapshotsResponse{Snapshots: list}, nil
}

// runStream 执行一个会输出过程的操作，把输出和结果发给面板。
func runStream(send func(*nagiv1.DeployEvent) error, op func(onLine func(string)) (int64, error)) error {
	version, err := op(func(line string) {
		_ = send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrConflict) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil, Version: version}
	if err != nil {
		result.Error = err.Error()
	}
	return send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) Rollback(req *nagiv1.RollbackRequest, stream nagiv1.AgentService_RollbackServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		return s.projects.Rollback(stream.Context(), req.GetId(), req.GetMember(), req.GetVersion(), req.GetRestoreData(), onLine)
	})
}

func (s *agentService) CreateSnapshot(req *nagiv1.CreateSnapshotRequest, stream nagiv1.AgentService_CreateSnapshotServer) error {
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		_, err := s.projects.TakeSnapshot(stream.Context(), req.GetId(), snapshot.Manual, onLine)
		return 0, err
	})
}

func (s *agentService) RestoreSnapshot(req *nagiv1.RestoreSnapshotRequest, stream nagiv1.AgentService_RestoreSnapshotServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		return 0, s.projects.RestoreSnapshot(stream.Context(), req.GetId(), req.GetSnapshotId(), req.GetMember(), onLine)
	})
}

func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, project.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, project.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, project.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// ---- 私有仓库认证 ----

func (s *agentService) ListRegistries(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.RegistriesResponse, error) {
	list, err := s.projects.Registries(ctx, req.GetProjectId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.RegistriesResponse{Registries: list}, nil
}

func (s *agentService) LoginRegistry(ctx context.Context, req *nagiv1.LoginRegistryRequest) (*nagiv1.RegistriesResponse, error) {
	list, err := s.projects.Login(ctx, req.GetProjectId(), req.GetRegistry(), req.GetUsername(), req.GetPassword())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.RegistriesResponse{Registries: list}, nil
}

func (s *agentService) LogoutRegistry(ctx context.Context, req *nagiv1.LogoutRegistryRequest) (*nagiv1.RegistriesResponse, error) {
	list, err := s.projects.Logout(ctx, req.GetProjectId(), req.GetRegistry())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.RegistriesResponse{Registries: list}, nil
}
