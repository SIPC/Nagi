package agent

import (
	"context"
	"errors"
	"io"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxUploads 是同时接收的大文件上传数（容器文件和节点文件合计）。都要先写到磁盘上暂存，多了直接拒绝，不排队。
const maxUploads = 2

var uploadSlots = make(chan struct{}, maxUploads)

// acquireUpload 占一个上传名额，返回的函数释放它。满了时返回 ResourceExhausted。
func acquireUpload() (func(), error) {
	select {
	case uploadSlots <- struct{}{}:
		return func() { <-uploadSlots }, nil
	default:
		return nil, status.Error(codes.ResourceExhausted, "节点正在接收其他上传，请稍后重试")
	}
}

// meshService 是节点在 mesh 上提供的服务。mesh 只说明网络能通，每个方法都要单独校验调用方。
type meshService struct {
	mesh.Service
	a *agent
}

// authorizePanel 检查经 mesh 调用 method 的 k 是不是节点现在信任的面板，修改只接受主面板。
// 面板的 mesh 公钥是它经控制连接告诉我们的，可信；但移除的面板的公钥为了网络连续性还会留一段时间
// （见 meshNode.prune），所以要再查信任列表。
func (a *agent) authorizePanel(k mesh.Key, method string) error {
	fp, ok := a.mesh.panelOf(k)
	if !ok || !a.id.trusts(fp) {
		return status.Error(codes.PermissionDenied, "仅接受受信任的面板")
	}
	return a.checkPrimary(fp, method)
}

// checkMeshPanel 检查 mesh 上来的只给面板的请求。primaryGuard 只挂在控制连接上，这里要自己查。
func (s *meshService) checkMeshPanel(ctx context.Context, method string) error {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return status.Error(codes.PermissionDenied, "仅接受受信任的面板")
	}
	return s.a.authorizePanel(caller, method)
}

// VolumeDownload 把卷里（或快照里）的文件发给面板。
func (s *meshService) VolumeDownload(req *nagiv1.VolumeFileRef, stream nagiv1.MeshService_VolumeDownloadServer) error {
	if err := s.checkMeshPanel(stream.Context(), nagiv1.MeshService_VolumeDownload_FullMethodName); err != nil {
		return err
	}
	return toStatus(s.a.projects.ReadVolumeFile(stream.Context(), req, stream.Send))
}

// VolumeUpload 把面板传来的文件写进卷里。有顶替的容器只在持有租约时能写。
func (s *meshService) VolumeUpload(stream nagiv1.MeshService_VolumeUploadServer) error {
	if err := s.checkMeshPanel(stream.Context(), nagiv1.MeshService_VolumeUpload_FullMethodName); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ref := first.GetRef()
	check := func() error { return s.a.checkFileWrite(ref.GetProjectId(), ref.GetContainer()) }
	if err := check(); err != nil {
		return toStatus(err)
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			c, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(c.GetData()); err != nil {
				return
			}
		}
	}()
	want := project.UploadCheck{ExpectedSHA256: first.GetExpectedSha256(), CreateOnly: first.GetCreateOnly()}
	n, err := s.a.projects.UploadVolumeFile(stream.Context(), ref, pr, want, check)
	pr.Close()
	if err != nil {
		return toStatus(err)
	}
	return stream.SendAndClose(&nagiv1.VolumeUploadResponse{Size: n})
}

func (s *meshService) Upload(stream nagiv1.MeshService_UploadServer) error {
	if err := s.checkMeshPanel(stream.Context(), nagiv1.MeshService_Upload_FullMethodName); err != nil {
		return err
	}
	release, err := acquireUpload()
	if err != nil {
		return err
	}
	defer release()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write(first.GetData()); err != nil {
			return
		}
		for {
			c, err := stream.Recv()
			if err == io.EOF {
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(c.GetData()); err != nil {
				return
			}
		}
	}()
	res, err := s.a.projects.ReceiveUpload(stream.Context(), first.GetProjectId(), first.GetContainer(), pr)
	pr.Close()
	if err != nil {
		return toStatus(err)
	}
	s.a.log.Info("收到上传的项目文件", "project", first.GetProjectId(), "container", first.GetContainer(), "bytes", res.Bytes, "files", res.Files)
	return stream.SendAndClose(&nagiv1.UploadResponse{Bytes: uint64(res.Bytes), Files: uint32(res.Files), Stripped: res.Stripped})
}
