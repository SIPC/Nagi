package agent

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
)

// 面板发来的操作开始执行以后就做完，不跟着调用方取消：用户点了删除、部署以后关掉页面（或者面板重启），
// 浏览器的请求被取消，这个取消会一路传到节点。删除、部署做到一半停下，会留下容器停了、项目还在这样的半截状态。
// 只是不再能把结果和输出送回去。
const (
	detachedUnaryTimeout  = 10 * time.Minute
	detachedStreamTimeout = 2 * time.Hour
)

func detachUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedUnaryTimeout)
	defer cancel()
	return handler(ctx, req)
}

func detachStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	// 日志、终端这类一直跟着看的流，对方走了就该停。
	if info.IsClientStream || isFollowStream(info.FullMethod) {
		return handler(srv, ss)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ss.Context()), detachedStreamTimeout)
	defer cancel()
	return handler(srv, &detachedStream{ServerStream: ss, ctx: ctx})
}

type detachedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *detachedStream) Context() context.Context { return s.ctx }

func isFollowStream(method string) bool {
	return strings.HasSuffix(method, "/StreamLogs")
}
