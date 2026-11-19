package nodes

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionPanel 是一个只有节点注册表的面板：接受节点的控制连接（TLS + yamux），给测试用。
type sessionPanel struct {
	r      *Registry
	q      *store.Queries
	server tls.Certificate
	ca     *tlsutil.CA
	nodeID string
	node   tls.Certificate
}

func newSessionPanel(t *testing.T) *sessionPanel {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	q := store.New(d)
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	skey, _ := tlsutil.GenerateKey()
	scert, err := ca.IssueServer(skey, []string{"panel"})
	if err != nil {
		t.Fatal(err)
	}
	nkey, _ := tlsutil.GenerateKey()
	csr, _ := tlsutil.CreateCSR(nkey, "n")
	ncert, err := ca.SignCSR(csr, "x", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := NodeID(ncert.RawSubjectPublicKeyInfo)
	if ncert, err = ca.SignCSR(csr, nodeID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: nodeID, Name: "n1", MeshKey: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	return &sessionPanel{
		r: NewRegistry(q, slog.Default(), nil, ca), q: q, ca: ca, nodeID: nodeID,
		server: tls.Certificate{Certificate: [][]byte{scert.Raw}, PrivateKey: skey},
		node:   tls.Certificate{Certificate: [][]byte{ncert.Raw}, PrivateKey: nkey},
	}
}

// nodeSession 是节点一端的一条控制连接。
type nodeSession struct {
	sess   *yamux.Session
	panel  nagiv1.PanelServiceClient
	closed chan struct{} // 面板一端的 Handle 返回时关闭
}

// connect 用节点的证书连一次面板，和节点的 agent 一样在会话上打开 gRPC 客户端。
func (p *sessionPanel) connect(t *testing.T) *nodeSession {
	t.Helper()
	a, b := net.Pipe()
	srvConn := tls.Server(a, &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.server},
		NextProtos: []string{tunnel.ALPN}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: p.ca.Pool(),
	})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if err := srvConn.HandshakeContext(context.Background()); err != nil {
			srvConn.Close()
			return
		}
		p.r.Handle(context.Background(), srvConn)
	}()
	cliConn := tls.Client(b, &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.node}, RootCAs: p.ca.Pool(),
		ServerName: "panel", NextProtos: []string{tunnel.ALPN},
	})
	if err := cliConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Client(cliConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	cc, err := tunnel.Dial(sess)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return &nodeSession{sess: sess, panel: nagiv1.NewPanelServiceClient(cc), closed: closed}
}

func (n *nodeSession) report(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := n.panel.Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: "h"}})
	return err
}

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s没有断开", what)
	}
}

// 同一个节点再连一次：旧连接立即断开，旧连接上的调用失败，新连接照常。
func TestReconnectClosesOldSession(t *testing.T) {
	p := newSessionPanel(t)
	first := p.connect(t)
	if err := first.report(t); err != nil {
		t.Fatalf("first report: %v", err)
	}
	second := p.connect(t)
	if err := second.report(t); err != nil {
		t.Fatalf("second report: %v", err)
	}
	waitClosed(t, "旧连接", first.sess.CloseChan())
	waitClosed(t, "旧连接的 Handle ", first.closed)
	if err := first.report(t); err == nil {
		t.Fatal("旧连接上的调用成功了")
	}
	c, err := p.r.Peek(p.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.report(t); err != nil {
		t.Fatalf("新连接: %v", err)
	}
	if c2, _ := p.r.Peek(p.nodeID); c2 != c {
		t.Fatal("当前连接变了")
	}
	p.r.mu.RLock()
	n := len(p.r.sessions[p.nodeID])
	p.r.mu.RUnlock()
	if n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

// 不是当前连接的调用一律拒绝，连接还没断开也一样。
func TestStaleConnRejected(t *testing.T) {
	r := NewRegistry(nil, slog.Default(), nil, nil)
	stale := &Conn{NodeID: "n"}
	cur := &Conn{NodeID: "n"}
	r.mu.Lock()
	r.addSession(stale)
	r.addSession(cur)
	r.mu.Unlock()

	called := false
	handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }
	unary, stream := r.onlyCurrent(stale)
	if _, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.Unavailable || called {
		t.Fatalf("stale unary: err=%v called=%v", err, called)
	}
	if err := stream(nil, nil, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { called = true; return nil }); status.Code(err) != codes.Unavailable || called {
		t.Fatalf("stale stream: err=%v called=%v", err, called)
	}
	unary, _ = r.onlyCurrent(cur)
	if _, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); err != nil || !called {
		t.Fatalf("current unary: err=%v called=%v", err, called)
	}
	// 断开以后，原来的当前连接上的调用也拒绝。
	r.Disconnect("n")
	called = false
	if _, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.Unavailable || called {
		t.Fatalf("after disconnect: err=%v called=%v", err, called)
	}
}

// Disconnect 断开节点所有的控制连接，包括还登记着的旧连接；移除以后节点连不上。
func TestDisconnectClosesAllSessions(t *testing.T) {
	p := newSessionPanel(t)
	live := p.connect(t)
	if err := live.report(t); err != nil {
		t.Fatal(err)
	}
	// 一条还登记着、没有断开的旧连接。
	var lingering atomic.Bool
	p.r.mu.Lock()
	p.r.sessions[p.nodeID][&Conn{NodeID: p.nodeID, close: func() { lingering.Store(true) }}] = struct{}{}
	p.r.mu.Unlock()

	if err := p.q.RevokeNode(context.Background(), p.nodeID); err != nil {
		t.Fatal(err)
	}
	p.r.Disconnect(p.nodeID)
	waitClosed(t, "当前连接", live.sess.CloseChan())
	waitClosed(t, "当前连接的 Handle ", live.closed)
	if !lingering.Load() {
		t.Fatal("旧连接没有断开")
	}
	if p.r.Online(p.nodeID) {
		t.Fatal("节点还在线")
	}
	if err := live.report(t); err == nil {
		t.Fatal("断开以后的调用成功了")
	}
	// 移除以后再连：面板拒绝，Handle 马上返回。
	again := p.connect(t)
	waitClosed(t, "移除以后的新连接", again.closed)
	if p.r.Online(p.nodeID) {
		t.Fatal("移除的节点又上线了")
	}
}
