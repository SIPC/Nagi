// Package tunnel 在一条 TLS 连接上用 yamux 复用出多条通道，并在上面跑双向的 gRPC：
// 两端各自既是 gRPC 服务端（处理对方打开的通道），也是 gRPC 客户端（自己打开通道）。
// 这样连接虽然由节点发起，面板也能像调用普通接口一样调用节点（反向 gRPC）。
package tunnel

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ALPN 是节点控制连接使用的 TLS 应用层协议名，面板据此把节点连接和浏览器连接分开。
const ALPN = "nagi-agent/1"

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// Server 在面板一端把连接包装成 yamux 会话。
func Server(conn net.Conn) (*yamux.Session, error) {
	return yamux.Server(conn, yamuxConfig())
}

// Client 在节点一端把连接包装成 yamux 会话。
func Client(conn net.Conn) (*yamux.Session, error) {
	return yamux.Client(conn, yamuxConfig())
}

// Serve 在会话上运行 gRPC 服务端，处理对方打开的通道，直到会话关闭。
// 外层已经是 TLS，所以通道内不再加密。
func Serve(session *yamux.Session, srv *grpc.Server) error {
	return srv.Serve(session)
}

// Dial 返回一个通过会话打开新通道来通信的 gRPC 客户端连接。
func Dial(session *yamux.Session) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///nagi-tunnel",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return session.Open()
		}),
	)
}
