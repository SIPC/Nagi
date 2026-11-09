package api

import (
	"context"
	"errors"
	"io"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc"
)

// MCP 读写卷里的文件（见 docs/mcp.md#文件）。和网页的文件管理走同一条路：经 mesh 传给容器所在的节点，节点用 os.Root
// 限制在卷里，顶替的容器只在持有租约时能写。权限和终端一样：能操作容器的角色、访问范围内的项目，令牌还要允许执行命令
// （能执行命令本来就能读写容器里的文件）。读写都记进活动记录，带着路径。

// ErrFileTooLarge 表示文件超过了读取的上限。
var ErrFileTooLarge = errors.New("file too large")

func (a *API) checkAgentFiles(ctx context.Context, projectID string, write bool) error {
	u := auth.CurrentUser(ctx)
	if !auth.CanOperate(u.Role) || u.TokenID != 0 && !u.AllowExec {
		return i18n.New("当前账号没有权限执行这个操作")
	}
	if err := a.projectAllowed(ctx, u, projectID); err != nil {
		return err
	}
	if write && a.Panel.Standby != nil {
		return a.standbyError()
	}
	return nil
}

// meshFiles 连上容器所在节点的 MeshService。
func (a *API) meshFiles(ctx context.Context, ref *nagiv1.VolumeFileRef) (nagiv1.MeshServiceClient, *grpc.ClientConn, error) {
	m := a.Nodes.Mesh()
	if m == nil {
		return nil, nil, i18n.New("面板的节点网络没有启动，无法传输文件")
	}
	nodeID, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return nil, nil, err
	}
	key, err := a.Nodes.NodeMeshKey(nodeID)
	if err != nil {
		return nil, nil, err
	}
	cc, err := mesh.Client(m, key)
	if err != nil {
		return nil, nil, err
	}
	return nagiv1.NewMeshServiceClient(cc), cc, nil
}

// ReadVolumeFile 读卷里的一个文件，超过 limit 字节返回 ErrFileTooLarge。目录不能读（网页下载时会打包，这里不要）。
func (a *API) ReadVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef, limit int64) ([]byte, error) {
	if err := a.checkAgentFiles(ctx, ref.GetProjectId(), false); err != nil {
		return nil, err
	}
	data, err := a.readVolumeFile(ctx, ref, limit)
	failure := ""
	if err != nil {
		failure = activityFailure(err)
	}
	a.recordOperation(ctx, "/DownloadVolumeFile", ref, failure)
	if err != nil && !errors.Is(err, ErrFileTooLarge) {
		return nil, fromGRPC(err)
	}
	return data, err
}

func (a *API) readVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef, limit int64) ([]byte, error) {
	client, cc, err := a.meshFiles(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.VolumeDownload(ctx, ref)
	if err != nil {
		return nil, err
	}
	head, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if head.GetArchive() {
		return nil, i18n.New("这是目录，不是文件")
	}
	if head.GetSize() > limit {
		return nil, ErrFileTooLarge
	}
	var out []byte
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, c.GetData()...)
		if int64(len(out)) > limit {
			return nil, ErrFileTooLarge
		}
	}
}

// WriteVolumeFile 把 data 写成卷里的一个文件（新建或整个覆盖）。节点先写临时文件再改名，沿用原文件的权限和属主。
// expectedSHA256 非空时，节点在写入前核对现在文件的 SHA-256，对不上返回 Aborted；createOnly 时文件已存在返回 AlreadyExists。
// 旧版本的节点不认识这两个字段，会直接写入，调用方要自己先检查一遍。
func (a *API) WriteVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef, data []byte, expectedSHA256 string, createOnly bool) error {
	if err := a.checkAgentFiles(ctx, ref.GetProjectId(), true); err != nil {
		return err
	}
	err := a.writeVolumeFile(ctx, &nagiv1.VolumeUploadChunk{Ref: ref, ExpectedSha256: expectedSHA256, CreateOnly: createOnly}, data)
	failure := ""
	if err != nil {
		failure = activityFailure(err)
	}
	a.recordOperation(ctx, "/UploadVolumeFile", ref, failure)
	if err != nil {
		return fromGRPC(err)
	}
	return nil
}

func (a *API) writeVolumeFile(ctx context.Context, first *nagiv1.VolumeUploadChunk, data []byte) error {
	client, cc, err := a.meshFiles(ctx, first.GetRef())
	if err != nil {
		return err
	}
	defer cc.Close()
	stream, err := client.VolumeUpload(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(first); err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(len(data), 256<<10)
		if err := stream.Send(&nagiv1.VolumeUploadChunk{Data: data[:n]}); err != nil {
			break // 节点提前结束（例如拒绝），真正的原因在 CloseAndRecv 里。
		}
		data = data[n:]
	}
	_, err = stream.CloseAndRecv()
	return err
}
