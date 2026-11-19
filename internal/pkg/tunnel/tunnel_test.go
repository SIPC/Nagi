package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
)

type panelSrv struct {
	nagiv1.UnimplementedPanelServiceServer
	got chan string
}

func (p *panelSrv) Report(_ context.Context, r *nagiv1.ReportRequest) (*nagiv1.ReportResponse, error) {
	p.got <- r.GetNode().GetHostname()
	return &nagiv1.ReportResponse{}, nil
}

type agentSrv struct {
	nagiv1.UnimplementedAgentServiceServer
}

func (agentSrv) GetMirrors(context.Context, *nagiv1.GetMirrorsRequest) (*nagiv1.Mirrors, error) {
	return &nagiv1.Mirrors{Mirrors: []string{"p1"}}, nil
}

// 两端在同一条连接上互相调用。
func TestBidirectionalGRPC(t *testing.T) {
	a, b := net.Pipe()
	panelSess, err := Server(a)
	if err != nil {
		t.Fatal(err)
	}
	agentSess, err := Client(b)
	if err != nil {
		t.Fatal(err)
	}
	defer panelSess.Close()
	defer agentSess.Close()

	ps := &panelSrv{got: make(chan string, 1)}
	pg := grpc.NewServer()
	nagiv1.RegisterPanelServiceServer(pg, ps)
	go Serve(panelSess, pg)

	ag := grpc.NewServer()
	nagiv1.RegisterAgentServiceServer(ag, agentSrv{})
	go Serve(agentSess, ag)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 节点调用面板
	ac, err := Dial(agentSess)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	if _, err := nagiv1.NewPanelServiceClient(ac).Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if h := <-ps.got; h != "h1" {
		t.Fatalf("hostname = %q", h)
	}

	// 面板反向调用节点
	pc, err := Dial(panelSess)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	resp, err := nagiv1.NewAgentServiceClient(pc).GetMirrors(ctx, &nagiv1.GetMirrorsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Mirrors) != 1 || resp.Mirrors[0] != "p1" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// pair 返回面板一端（运行 gRPC 服务端）和节点一端的会话。
func pair(t *testing.T) (panel, agent *yamux.Session, ps *panelSrv) {
	t.Helper()
	a, b := net.Pipe()
	panel, err := Server(a)
	if err != nil {
		t.Fatal(err)
	}
	agent, err = Client(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { panel.Close(); agent.Close() })
	ps = &panelSrv{got: make(chan string, 16)}
	pg := grpc.NewServer()
	nagiv1.RegisterPanelServiceServer(pg, ps)
	go Serve(panel, pg)
	t.Cleanup(pg.Stop)
	return panel, agent, ps
}

func report(t *testing.T, sess *yamux.Session) error {
	t.Helper()
	cc, err := Dial(sess)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = nagiv1.NewPanelServiceClient(cc).Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: "h"}})
	return err
}

// 对方同时打开的通道超出上限：断开整个会话。上限以内照常调用。
func TestTooManyStreamsClosesSession(t *testing.T) {
	panel, agent, _ := pair(t)
	// 占住 MaxStreams-1 个通道（只开不发，面板的 gRPC 在等 HTTP/2 前言）。
	for range MaxStreams - 1 {
		if _, err := agent.Open(); err != nil {
			t.Fatal(err)
		}
	}
	// 第 MaxStreams 个通道是正常的 gRPC 连接。
	if err := report(t, agent); err != nil {
		t.Fatalf("within limit: %v", err)
	}
	if panel.IsClosed() {
		t.Fatal("上限以内会话被断开")
	}
	// report 关掉了它的通道，等面板让出那一格，再开两个：第二个超出上限。
	waitActive(t, panel, agent, MaxStreams-1)
	for range 2 {
		if _, err := agent.Open(); err != nil {
			break
		}
	}
	select {
	case <-panel.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("超出上限后面板没有断开会话")
	}
	select {
	case <-agent.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("节点一端没有察觉会话断开")
	}
}

// waitActive 等到面板一端还开着的通道数是 n（yamux 两端各自计数，节点一端也要关闭完）。
func waitActive(t *testing.T, panel, agent *yamux.Session, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for panel.NumStreams() != n || agent.NumStreams() != n {
		if time.Now().After(deadline) {
			t.Fatalf("streams: panel=%d agent=%d, want %d", panel.NumStreams(), agent.NumStreams(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 关闭的通道让出名额：反复开关不受同时打开数的限制。
func TestClosedStreamsReleaseSlots(t *testing.T) {
	panel, agent, _ := pair(t)
	for i := range MaxStreams * 3 {
		if err := report(t, agent); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
		waitActive(t, panel, agent, 0)
	}
	if panel.IsClosed() {
		t.Fatal("会话被断开")
	}
}

// 一分钟内新开的通道超出上限：断开会话。过了这一分钟重新计数。
func TestStreamRateLimit(t *testing.T) {
	a, b := net.Pipe()
	panel, _ := Server(a)
	agent, _ := Client(b)
	defer panel.Close()
	defer agent.Close()
	now := time.Unix(1000, 0)
	l := newListener(panel, MaxStreams, 3)
	l.nowFunc = func() time.Time { return now }
	accept := func() error {
		s, err := agent.Open()
		if err != nil {
			return err
		}
		defer s.Close()
		c, err := l.Accept()
		if err != nil {
			return err
		}
		return c.Close()
	}
	for i := range 3 {
		if err := accept(); err != nil {
			t.Fatalf("accept %d: %v", i, err)
		}
	}
	now = now.Add(time.Minute)
	for i := range 3 {
		if err := accept(); err != nil {
			t.Fatalf("next minute accept %d: %v", i, err)
		}
	}
	if err := accept(); err != ErrTooManyStreams {
		t.Fatalf("over rate: %v", err)
	}
	if !panel.IsClosed() {
		t.Fatal("超出频率后会话没有断开")
	}
}
