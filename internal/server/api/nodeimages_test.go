package api

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 旧版本的节点没有镜像接口：面板提示升级节点（界面按 NodeInfo.image_ops 隐藏入口）。
func TestNodeImagesOldNode(t *testing.T) {
	err := nodeAccessError(status.Error(codes.Unimplemented, "unknown method ListImages"))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("应返回 FailedPrecondition：%v", err)
	}
	if got := ce.Message(); got != "节点版本过低，升级后才能使用此功能" {
		t.Errorf("提示 = %q", got)
	}
}

// 节点返回的每一项错误按请求的语言显示。
func TestLocalizeImageResults(t *testing.T) {
	list := []*nagiv1.ImageDeleteResult{{Error: "镜像正在被容器 web-1 使用"}, {Error: "Nagi 需要这个镜像，不能删除"}, {}}
	localizeImageResults(i18n.WithLang(context.Background(), i18n.En), list)
	if list[0].GetError() != "The image is used by container web-1" || list[1].GetError() != "Nagi needs this image, so it can't be deleted" || list[2].GetError() != "" {
		t.Errorf("翻译 = %v", list)
	}
}
