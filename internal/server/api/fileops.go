package api

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 文件的改名、移动、复制、压缩、解压和目录大小（见 docs/agent.md#存储、docs/node-access.md#文件）：
// 都在节点本地进行，面板只转发请求和进度。卷里的写操作和上传一样，特权容器只有管理员能动；
// 节点文件只有管理员，要先做高权限确认（elevationRequired）。拦截器把每次调用和参数里的路径记进审计。

// fileOpError 转换节点返回的错误。旧版本的节点没有这些接口。
func fileOpError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点版本过低，升级后才能使用此功能"))
	}
	return toConnect(err)
}

type fileOpStream interface {
	Recv() (*nagiv1.FileOpEvent, error)
}

// forwardFileOp 返回的函数把节点的进度和结果原样转给浏览器。写成 forwardFileOp(out)(agent.X(ctx, req))。
func forwardFileOp(out *connect.ServerStream[nagiv1.FileOpEvent]) func(fileOpStream, error) error {
	return func(stream fileOpStream, err error) error {
		if err != nil {
			return fileOpError(err)
		}
		for {
			ev, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fileOpError(err)
			}
			if err := out.Send(ev); err != nil {
				return err
			}
		}
	}
}

// drainFileOp 等节点做完，返回结果（改名很快，不用转发进度）。
func drainFileOp(stream fileOpStream, err error) (*nagiv1.FileOpResult, error) {
	if err != nil {
		return nil, fileOpError(err)
	}
	res := &nagiv1.FileOpResult{}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return nil, fileOpError(err)
		}
		if ev.GetResult() != nil {
			res = ev.GetResult()
		}
	}
}

// volumeWriteAgent 检查能不能改这个容器的文件，返回容器所在节点。
func (s *projectService) volumeWriteAgent(ctx context.Context, projectID, container string) (nagiv1.AgentServiceClient, error) {
	if err := s.checkPrivilegedWrite(ctx, auth.CurrentUser(ctx).Role, projectID, container); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	agent, err := s.Projects.ContainerAgent(ctx, projectID, container)
	if err != nil {
		return nil, toConnect(err)
	}
	return agent, nil
}

func (s *projectService) RenameVolumeFile(ctx context.Context, req *connect.Request[nagiv1.VolumeTransferRequest]) (*connect.Response[nagiv1.FileOpResult], error) {
	m := proto.CloneOf(req.Msg)
	m.Copy = false
	if len(m.GetPaths()) != 1 || m.GetNewName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请填写新的名称"))
	}
	agent, err := s.volumeWriteAgent(ctx, m.GetProjectId(), m.GetContainer())
	if err != nil {
		return nil, err
	}
	res, err := drainFileOp(agent.TransferVolumeFiles(ctx, m))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (s *projectService) transferVolume(ctx context.Context, msg *nagiv1.VolumeTransferRequest, isCopy bool, out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	m := proto.CloneOf(msg)
	m.Copy, m.NewName = isCopy, ""
	agent, err := s.volumeWriteAgent(ctx, m.GetProjectId(), m.GetContainer())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(agent.TransferVolumeFiles(ctx, m))
}

func (s *projectService) MoveVolumeFiles(ctx context.Context, req *connect.Request[nagiv1.VolumeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	return s.transferVolume(ctx, req.Msg, false, out)
}

func (s *projectService) CopyVolumeFiles(ctx context.Context, req *connect.Request[nagiv1.VolumeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	return s.transferVolume(ctx, req.Msg, true, out)
}

func (s *projectService) CompressVolumeFiles(ctx context.Context, req *connect.Request[nagiv1.VolumeCompressRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	agent, err := s.volumeWriteAgent(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(agent.CompressVolumeFiles(ctx, req.Msg))
}

func (s *projectService) ExtractVolumeFile(ctx context.Context, req *connect.Request[nagiv1.VolumeExtractRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	agent, err := s.volumeWriteAgent(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(agent.ExtractVolumeFile(ctx, req.Msg))
}

func (s *projectService) ListVolumeDirSizes(ctx context.Context, req *connect.Request[nagiv1.VolumeDirSizesRequest]) (*connect.Response[nagiv1.DirSizesResponse], error) {
	agent, err := s.Projects.ContainerAgent(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer())
	if err != nil {
		return nil, toConnect(err)
	}
	out, err := agent.ListVolumeDirSizes(ctx, req.Msg)
	if err != nil {
		return nil, fileOpError(err)
	}
	return connect.NewResponse(out), nil
}

// ---- 节点文件 ----

func (s *nodeService) RenameNodeFile(ctx context.Context, req *connect.Request[nagiv1.NodeTransferRequest]) (*connect.Response[nagiv1.FileOpResult], error) {
	m := proto.CloneOf(req.Msg)
	m.Copy = false
	if len(m.GetPaths()) != 1 || m.GetNewName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请填写新的名称"))
	}
	a, err := s.agent(m.GetNodeId())
	if err != nil {
		return nil, err
	}
	res, err := drainFileOp(a.TransferNodeFiles(ctx, m))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (s *nodeService) transferNode(ctx context.Context, msg *nagiv1.NodeTransferRequest, isCopy bool, out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	m := proto.CloneOf(msg)
	m.Copy, m.NewName = isCopy, ""
	a, err := s.agent(m.GetNodeId())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(a.TransferNodeFiles(ctx, m))
}

func (s *nodeService) MoveNodeFiles(ctx context.Context, req *connect.Request[nagiv1.NodeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	return s.transferNode(ctx, req.Msg, false, out)
}

func (s *nodeService) CopyNodeFiles(ctx context.Context, req *connect.Request[nagiv1.NodeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	return s.transferNode(ctx, req.Msg, true, out)
}

func (s *nodeService) CompressNodeFiles(ctx context.Context, req *connect.Request[nagiv1.NodeCompressRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(a.CompressNodeFiles(ctx, req.Msg))
}

func (s *nodeService) ExtractNodeFile(ctx context.Context, req *connect.Request[nagiv1.NodeExtractRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	return forwardFileOp(out)(a.ExtractNodeFile(ctx, req.Msg))
}

func (s *nodeService) ListNodeDirSizes(ctx context.Context, req *connect.Request[nagiv1.NodeDirSizesRequest]) (*connect.Response[nagiv1.DirSizesResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.ListNodeDirSizes(ctx, req.Msg)
	if err != nil {
		return nil, fileOpError(err)
	}
	return connect.NewResponse(out), nil
}
