package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
		return nil, status.Errorf(codes.Internal, "无法保存告警配置：%v", err)
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
	data = metrics.Aggregate(data, time.Duration(req.GetStep())*time.Second)
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
			return nil, status.Error(codes.InvalidArgument, "项目根目录须为绝对路径，且不能是 /")
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
	// 计划里的跨节点入口变了，Caddy 也要跟着变。
	s.changed()
	return &nagiv1.SetLinksResponse{}, nil
}

func (s *agentService) CheckPorts(ctx context.Context, req *nagiv1.CheckPortsRequest) (*nagiv1.CheckPortsResponse, error) {
	if err := s.projects.CheckPorts(ctx, req.GetRoutes()); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.CheckPortsResponse{}, nil
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
	var err error
	if req.GetRetire() {
		var dir string
		if dir, err = s.projects.Retire(ctx, req.GetProjectId(), req.GetContainer(), func(string) {}); err == nil {
			s.node.log.Info("容器已在其他节点上恢复，停止并保留目录", "project", req.GetProjectId(), "container", req.GetContainer(), "dir", dir)
		}
	} else {
		err = s.projects.Delete(ctx, req.GetProjectId(), req.GetContainer(), req.GetDeleteDir(), func(string) {})
	}
	s.changed()
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.DeleteContainerResponse{}, nil
}

func (s *agentService) ControlContainer(req *nagiv1.ControlContainerRequest, stream nagiv1.AgentService_ControlContainerServer) error {
	return runStream(stream.Send, func(onLine func(string)) error {
		if err := s.node.checkControl(req.GetProjectId(), req.GetContainer(), req.GetAction()); err != nil {
			return err
		}
		// 先通知上报再返回结果，页面拿到结果时状态已在更新。
		defer s.changed()
		return s.projects.Control(stream.Context(), req.GetProjectId(), req.GetContainer(), req.GetAction(), onLine)
	})
}

func (s *agentService) ListVolumes(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.ListVolumesResponse, error) {
	out, err := s.projects.ListVolumes(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, toStatus(err)
	}
	return out, nil
}

func (s *agentService) ListVolumeFiles(ctx context.Context, req *nagiv1.VolumeFileRef) (*nagiv1.ListVolumeFilesResponse, error) {
	out, err := s.projects.ListVolumeFiles(ctx, req)
	if err != nil {
		return nil, toStatus(err)
	}
	return out, nil
}

// checkVolumeWrite：有顶替的容器只在持有租约时能改文件，规则和启动、重启一样。
func (s *agentService) checkVolumeWrite(ref *nagiv1.VolumeFileRef) error {
	return s.node.checkFileWrite(ref.GetProjectId(), ref.GetContainer())
}

func (s *agentService) DeleteVolumeFile(ctx context.Context, req *nagiv1.VolumeFileRef) (*nagiv1.VolumeFileOpResponse, error) {
	if err := s.checkVolumeWrite(req); err != nil {
		return nil, toStatus(err)
	}
	if err := s.projects.DeleteVolumeFile(ctx, req); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.VolumeFileOpResponse{}, nil
}

func (s *agentService) CreateVolumeDir(ctx context.Context, req *nagiv1.VolumeFileRef) (*nagiv1.VolumeFileOpResponse, error) {
	if err := s.checkVolumeWrite(req); err != nil {
		return nil, toStatus(err)
	}
	if err := s.projects.CreateVolumeDir(ctx, req); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.VolumeFileOpResponse{}, nil
}

func (s *agentService) RestoreVolumeFile(ctx context.Context, req *nagiv1.VolumeFileRef) (*nagiv1.VolumeFileOpResponse, error) {
	if err := s.checkVolumeWrite(req); err != nil {
		return nil, toStatus(err)
	}
	if err := s.projects.RestoreVolumeFile(ctx, req); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.VolumeFileOpResponse{}, nil
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

func (s *agentService) DeleteSnapshot(ctx context.Context, req *nagiv1.RestoreSnapshotRequest) (*nagiv1.VolumeFileOpResponse, error) {
	if err := s.node.checkFileWrite(req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, toStatus(err)
	}
	if err := s.projects.DeleteSnapshot(ctx, req.GetProjectId(), req.GetContainer(), req.GetSnapshotId()); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.VolumeFileOpResponse{}, nil
}

// ReclaimSnapshots 立即清理本节点上每个容器的快照仓库（节点设置里的「清理快照仓库」）。逐个容器做，
// 一个失败不影响其他的；本节点没有持有顶替租约的容器跳过。开始了就做完，浏览器断开也不中断。
func (s *agentService) ReclaimSnapshots(_ *nagiv1.ReclaimSnapshotsRequest, stream nagiv1.AgentService_ReclaimSnapshotsServer) error {
	ctx := context.WithoutCancel(stream.Context())
	return runStream(stream.Send, func(onLine func(string)) error {
		var total int64
		var done int
		var failed []string
		for _, c := range s.projects.List(ctx) {
			sp := c.Spec
			if sp.GetVersion() == 0 {
				continue
			}
			name := s.projects.ProjectName(ctx, sp.GetProjectId()) + " / " + sp.GetName()
			if err := s.node.checkFileWrite(sp.GetProjectId(), sp.GetName()); err != nil {
				onLine(name + "：跳过，" + err.Error())
				continue
			}
			onLine(name + "：正在清理")
			freed, err := s.projects.ReclaimSpace(ctx, sp.GetProjectId(), sp.GetName())
			total += freed
			if err != nil {
				onLine(name + "：失败，" + err.Error())
				failed = append(failed, name)
				continue
			}
			done++
			onLine(name + "：释放 " + humanBytes(freed))
		}
		onLine(fmt.Sprintf("共清理 %d 个容器，释放 %s", done, humanBytes(total)))
		if len(failed) > 0 {
			return fmt.Errorf("这些容器清理失败：%s", strings.Join(failed, "、"))
		}
		return nil
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

// GetCache 返回本节点上的全部缓存，面板据此把项目拼回来。
func (s *agentService) GetCache(ctx context.Context, _ *nagiv1.GetCacheRequest) (*nagiv1.NodeCache, error) {
	out := &nagiv1.NodeCache{Failovers: s.node.failoverStatuses(ctx), Links: s.node.intranet.current()}
	for _, c := range s.projects.List(ctx) {
		out.Containers = append(out.Containers, c.Spec)
	}
	return out, nil
}
