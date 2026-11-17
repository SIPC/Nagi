package agent

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

// 修改类的请求不跟着调用方取消；目录大小的统计跟着取消，浏览器离开页面后节点就停下、让出完整统计的名额。
func TestDetachUnary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := func(method string) bool {
		var got bool
		_, _ = detachUnary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
			got = ctx.Err() != nil
			return nil, nil
		})
		return got
	}
	if canceled(nagiv1.AgentService_DeleteContainer_FullMethodName) {
		t.Error("删除容器不应跟着调用方取消")
	}
	for _, m := range []string{nagiv1.AgentService_ListVolumeDirSizes_FullMethodName, nagiv1.AgentService_ListNodeDirSizes_FullMethodName} {
		if !canceled(m) {
			t.Errorf("%s 应跟着调用方取消", m)
		}
	}
}
