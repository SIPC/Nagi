package agent

import (
	"io"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// meshService 是节点在 mesh 上提供的服务。mesh 只说明网络能通，每个方法都要单独校验调用方。
type meshService struct {
	mesh.Service
	a *agent
}

// isPanel 判断调用方是不是节点信任的面板。面板的 mesh 公钥是它经控制连接告诉我们的，可信。
func (n *meshNode) isPanel(k mesh.Key) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, pk := range n.panelKeys {
		if pk == k {
			return true
		}
	}
	return false
}

func (s *meshService) Upload(stream nagiv1.MeshService_UploadServer) error {
	caller, ok := mesh.Caller(stream.Context(), s.M)
	if !ok || !s.a.mesh.isPanel(caller) {
		return status.Error(codes.PermissionDenied, "只接受信任的面板上传文件")
	}
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
	res, err := s.a.projects.ReceiveUpload(stream.Context(), first.GetProjectId(), pr)
	pr.Close()
	if err != nil {
		return toStatus(err)
	}
	s.a.log.Info("收到上传的项目文件", "project", first.GetProjectId(), "bytes", res.Bytes, "files", res.Files)
	return stream.SendAndClose(&nagiv1.UploadResponse{Bytes: uint64(res.Bytes), Files: uint32(res.Files), Stripped: res.Stripped})
}
