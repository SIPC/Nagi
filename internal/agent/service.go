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

func (s *agentService) SetProjectsRoot(_ context.Context, req *nagiv1.SetProjectsRootRequest) (*nagiv1.SetProjectsRootResponse, error) {
	if err := s.node.setProjectsRoot(req.GetRoot()); err != nil {
		if errors.Is(err, project.ErrInvalidArgument) {
			return nil, status.Error(codes.InvalidArgument, "项目根目录必须是绝对路径，且不能是 /")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.changed()
	return &nagiv1.SetProjectsRootResponse{}, nil
}

func (s *agentService) SetLinks(_ context.Context, req *nagiv1.LinkPlan) (*nagiv1.SetLinksResponse, error) {
	if err := s.node.intranet.set(req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &nagiv1.SetLinksResponse{}, nil
}

// ---- 容器 ----

func (s *agentService) PutContainer(ctx context.Context, req *nagiv1.PutContainerRequest) (*nagiv1.PutContainerResponse, error) {
	if err := s.projects.Put(ctx, req.GetSpec()); err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.PutContainerResponse{}, nil
}

func (s *agentService) DeployContainer(req *nagiv1.DeployContainerRequest, stream nagiv1.AgentService_DeployContainerServer) error {
	defer s.changed()
	opts := project.DeployOptions{
		Pull: req.GetPull(), Upload: req.GetUseUpload(), PullImages: req.GetPullImages(),
		FilesFrom: req.GetFilesFrom(), RestoreDataFrom: req.GetRestoreDataFrom(), Previous: req.GetPreviousVersion(),
	}
	send := func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	}
	res, err := s.projects.Deploy(stream.Context(), req.GetSpec(), opts, send)
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{
		Success: err == nil, Version: req.GetSpec().GetVersion(),
		Images: res.Images, Commit: res.Commit, HasFiles: res.HasFiles,
	}
	if err != nil {
		result.Error = explain(err.Error())
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) DeleteContainer(ctx context.Context, req *nagiv1.DeleteContainerRequest) (*nagiv1.DeleteContainerResponse, error) {
	err := s.projects.Delete(ctx, req.GetProjectId(), req.GetContainer(), req.GetDeleteDir(), func(string) {})
	s.changed()
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.DeleteContainerResponse{}, nil
}

func (s *agentService) ListSnapshots(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.ListSnapshotsResponse, error) {
	list, err := s.projects.Snapshots(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListSnapshotsResponse{Snapshots: list}, nil
}

func (s *agentService) CreateSnapshot(req *nagiv1.ContainerRef, stream nagiv1.AgentService_CreateSnapshotServer) error {
	return runStream(stream.Send, func(onLine func(string)) error {
		_, err := s.projects.TakeSnapshot(stream.Context(), req.GetProjectId(), req.GetContainer(), snapshot.Manual, onLine)
		return err
	})
}

func (s *agentService) RestoreSnapshot(req *nagiv1.RestoreSnapshotRequest, stream nagiv1.AgentService_RestoreSnapshotServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) error {
		return s.projects.RestoreSnapshot(stream.Context(), req.GetProjectId(), req.GetContainer(), req.GetSnapshotId(), onLine)
	})
}

func (s *agentService) LoginRegistry(ctx context.Context, req *nagiv1.LoginRegistryRequest) (*nagiv1.LoginRegistryResponse, error) {
	cfg, err := s.projects.Login(ctx, req.GetRegistry(), req.GetUsername(), req.GetPassword())
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.LoginRegistryResponse{DockerConfig: cfg}, nil
}

func (s *agentService) StreamLogs(req *nagiv1.StreamLogsRequest, stream nagiv1.AgentService_StreamLogsServer) error {
	err := s.projects.Logs(stream.Context(), req.GetProjectId(), req.GetContainer(), req.GetService(), int(req.GetTail()), req.GetFollow(), func(l docker.LogLine) {
		_ = stream.Send(&nagiv1.LogLine{
			Container: l.Container, Service: l.Service, Stream: l.Stream, Text: l.Text, Time: timestamppb.New(l.Time),
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
	ex, err := s.projects.Exec(ctx, start.GetProjectId(), start.GetContainer(), start.GetService(), start.GetCommand(), uint(size.GetCols()), uint(size.GetRows()))
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

// runStream 执行一个会输出过程的操作，把输出和结果发给面板。
func runStream(send func(*nagiv1.DeployEvent) error, op func(onLine func(string)) error) error {
	err := op(func(line string) {
		_ = send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	result := &nagiv1.DeployResult{Success: err == nil}
	if err != nil {
		result.Error = explain(err.Error())
	}
	return send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, project.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, project.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
