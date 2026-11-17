package api

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 节点镜像（见 docs/agent.md#节点镜像）：只有管理员能用（ListNodeImages 登记在 auth 的 adminReads，NodeService 的修改默认只给管理员），
// 访问令牌不能用（auth.TokenAllowed）。能不能删、清理哪些由节点判断，面板只转发。

// maxImageBatch 是一次最多删除、清理的镜像数，节点也有同样的限制。
const maxImageBatch = 1000

func (s *nodeService) ListNodeImages(ctx context.Context, req *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.NodeImages], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.ListImages(ctx, &nagiv1.ListImagesRequest{})
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

// localizeImageResults 按请求的语言翻译节点返回的每一项错误。
func localizeImageResults(ctx context.Context, list []*nagiv1.ImageDeleteResult) {
	lang := i18n.FromContext(ctx)
	for _, r := range list {
		if r.GetError() != "" {
			r.Error = i18n.Text(lang, r.GetError())
		}
	}
}

func (s *nodeService) DeleteNodeImages(ctx context.Context, req *connect.Request[nagiv1.DeleteNodeImagesRequest]) (*connect.Response[nagiv1.DeleteImagesResponse], error) {
	m := req.Msg
	if len(m.GetImages()) == 0 || len(m.GetImages()) > maxImageBatch {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("一次可删除 1 至 %d 个镜像", maxImageBatch))
	}
	a, err := s.agent(m.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.DeleteImages(ctx, &nagiv1.DeleteImagesRequest{Images: m.GetImages()})
	if err != nil {
		return nil, nodeAccessError(err)
	}
	localizeImageResults(ctx, out.GetResults())
	return connect.NewResponse(out), nil
}

func (s *nodeService) PruneNodeImages(ctx context.Context, req *connect.Request[nagiv1.PruneNodeImagesRequest]) (*connect.Response[nagiv1.PruneImagesResponse], error) {
	m := req.Msg
	if len(m.GetIds()) > maxImageBatch {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("一次最多清理 %d 个镜像", maxImageBatch))
	}
	a, err := s.agent(m.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.PruneImages(ctx, &nagiv1.PruneImagesRequest{Ids: m.GetIds(), BuildCache: m.GetBuildCache()})
	if err != nil {
		return nil, nodeAccessError(err)
	}
	localizeImageResults(ctx, out.GetFailed())
	return connect.NewResponse(out), nil
}

// PullNodeImage 把节点的拉取进度转给浏览器。和别的节点操作不同，浏览器断开（取消）时停止拉取：ctx 一路传到节点。
func (s *nodeService) PullNodeImage(ctx context.Context, req *connect.Request[nagiv1.PullNodeImageRequest], out *connect.ServerStream[nagiv1.ImagePullEvent]) error {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := a.PullImage(ctx, &nagiv1.PullImageRequest{Image: req.Msg.GetImage()})
	if err != nil {
		return nodeAccessError(err)
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nodeAccessError(err)
		}
		if err := out.Send(ev); err != nil {
			return err
		}
	}
}
