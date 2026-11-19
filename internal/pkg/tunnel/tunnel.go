// Package tunnel 在一条 TLS 连接上用 yamux 复用出多条通道，并在上面跑双向的 gRPC：
// 两端各自既是 gRPC 服务端（处理对方打开的通道），也是 gRPC 客户端（自己打开通道）。
// 这样连接虽然由节点发起，面板也能像调用普通接口一样调用节点（反向 gRPC）。
package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ALPN 是节点控制连接使用的 TLS 应用层协议名，面板据此把节点连接和浏览器连接分开。
const ALPN = "nagi-agent/1"

const (
	// MaxStreams 是一端同时接受对方打开的通道数上限。每个通道都是一条 gRPC 连接，
	// 对方实际只用一条（Dial 返回的客户端连接），gRPC 重连时新旧两条可能短暂同时存在。
	MaxStreams = 4
	// maxStreamsPerMinute 是一分钟内最多接受的通道数。正常情况下只有 gRPC 重连才开新通道；
	// 反复开了又关的通道在 yamux 里还要留到 StreamCloseTimeout，不限总数也能耗尽内存。
	maxStreamsPerMinute = 30
	// acceptBacklog 是 yamux 里还没有被接受的通道数上限，超出的直接重置。
	acceptBacklog = 16
)

// ErrTooManyStreams 表示对方打开的通道超出上限，会话已经断开。
var ErrTooManyStreams = errors.New("对方打开的通道过多，已断开")

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = acceptBacklog
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// Server 在面板一端把连接包装成 yamux 会话。对方发来的 ping 超出频率时断开（见 pingGuard）。
func Server(conn net.Conn) (*yamux.Session, error) {
	return yamux.Server(newPingGuard(conn), yamuxConfig())
}

// Client 在节点一端把连接包装成 yamux 会话。对方发来的 ping 超出频率时断开（见 pingGuard）。
func Client(conn net.Conn) (*yamux.Session, error) {
	return yamux.Client(newPingGuard(conn), yamuxConfig())
}

// Serve 在会话上运行 gRPC 服务端，处理对方打开的通道，直到会话关闭。
// 外层已经是 TLS，所以通道内不再加密。对方打开的通道超出上限时断开整个会话。
func Serve(session *yamux.Session, srv *grpc.Server) error {
	return srv.Serve(newListener(session, MaxStreams, maxStreamsPerMinute))
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

// listener 把会话当作 gRPC 的监听器，限制同时打开的通道数和每分钟新开的通道数。
type listener struct {
	*yamux.Session
	active chan struct{} // 每个还没关闭的通道占一格
	perMin int

	mu      sync.Mutex
	window  time.Time
	opened  int
	nowFunc func() time.Time
}

func newListener(s *yamux.Session, max, perMin int) *listener {
	return &listener{Session: s, active: make(chan struct{}, max), perMin: perMin, nowFunc: time.Now}
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Session.Accept()
	if err != nil {
		return nil, err
	}
	if !l.allow() {
		c.Close()
		l.Session.Close()
		return nil, ErrTooManyStreams
	}
	select {
	case l.active <- struct{}{}:
		return &stream{Conn: c, release: func() { <-l.active }}, nil
	default:
		c.Close()
		l.Session.Close()
		return nil, ErrTooManyStreams
	}
}

// allow 记一次新开的通道，返回这一分钟内是否还没超出上限。
func (l *listener) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc()
	if now.Sub(l.window) >= time.Minute {
		l.window, l.opened = now, 0
	}
	l.opened++
	return l.opened <= l.perMin
}

// stream 在关闭时让出占用的一格。gRPC 可能关闭不止一次。
type stream struct {
	net.Conn
	once    sync.Once
	release func()
}

func (s *stream) Close() error {
	err := s.Conn.Close()
	s.once.Do(s.release)
	return err
}
