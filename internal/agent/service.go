package agent

import (
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
	p, err := s.projects.Create(ctx, req.GetName(), req.GetDir(), req.GetConfig())
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

func (s *agentService) SaveDraft(ctx context.Context, req *nagiv1.SaveDraftRequest) (*nagiv1.SaveDraftResponse, error) {
	p, err := s.projects.SaveDraft(ctx, req.GetId(), req.GetBaseRevision(), req.GetConfig())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.SaveDraftResponse{Project: p}, nil
}

func (s *agentService) DiscardDraft(ctx context.Context, req *nagiv1.DiscardDraftRequest) (*nagiv1.DiscardDraftResponse, error) {
	p, err := s.projects.DiscardDraft(ctx, req.GetId(), req.GetBaseRevision())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.DiscardDraftResponse{Project: p}, nil
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
	ctx := stream.Context()
	send := func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	}
	version, err := s.projects.Deploy(ctx, req.GetId(), req.GetBaseRevision(), req.GetPull(), req.GetUseUpload(), send)
	s.changed()
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrConflict) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil, Version: version}
	if err != nil {
		result.Error = err.Error()
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) StreamLogs(req *nagiv1.StreamLogsRequest, stream nagiv1.AgentService_StreamLogsServer) error {
	err := s.projects.Logs(stream.Context(), req.GetId(), req.GetService(), int(req.GetTail()), req.GetFollow(), func(l docker.LogLine) {
		_ = stream.Send(&nagiv1.LogLine{
			Service: l.Service, Stream: l.Stream, Text: l.Text, Time: timestamppb.New(l.Time),
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
	ex, err := s.projects.Exec(ctx, start.GetProjectId(), start.GetService(), start.GetCommand(), uint(size.GetCols()), uint(size.GetRows()))
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
	list, err := s.projects.Versions(ctx, req.GetId())
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
		return s.projects.Rollback(stream.Context(), req.GetId(), req.GetVersion(), req.GetRestoreData(), onLine)
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
		return 0, s.projects.RestoreSnapshot(stream.Context(), req.GetId(), req.GetSnapshotId(), onLine)
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
