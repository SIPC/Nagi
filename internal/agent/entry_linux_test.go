//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 端到端测试跨节点入口：本地转发口 → mesh → 目标节点 → "容器"（本机回显）。
// 只把找容器（FindEndpoint）换成假的，其余都是真的代码和真的 mesh。

// tcpEcho 在 ip:port 上开 TCP 回显，回"标记:内容"。port 为 0 时随机选。
func tcpEcho(t *testing.T, ip netip.Addr, port uint16, tag string) uint16 {
	t.Helper()
	ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				n, err := c.Read(buf)
				if err == nil {
					fmt.Fprintf(c, "%s:%s", tag, buf[:n])
				}
			}()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func connectMesh(t *testing.T, nodes ...*testNodeAgent) {
	t.Helper()
	for i, x := range nodes {
		var peers []mesh.Peer
		for j, y := range nodes {
			if i != j {
				peers = append(peers, mesh.Peer{Key: y.m.PublicKey(), Endpoints: []netip.AddrPort{
					netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), y.m.Port())}})
			}
		}
		if err := x.m.SetPeers(peers); err != nil {
			t.Fatal(err)
		}
	}
	for _, y := range nodes[1:] {
		deadline := time.Now().Add(15 * time.Second)
		for {
			direct := false
			for _, s := range nodes[0].m.Status() {
				direct = direct || (s.Key == y.m.PublicKey() && s.Path == "direct")
			}
			if direct {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("mesh 直连没有建立")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func linkNode(n *testNodeAgent) *nagiv1.LinkNode {
	k := n.m.PublicKey()
	return &nagiv1.LinkNode{Key: k[:], Name: "n"}
}

func routeNamed(routes []proxy.Route, name string) proxy.Route {
	for _, r := range routes {
		if r.Name == name {
			return r
		}
	}
	return proxy.Route{}
}

// tcpAsk 连到 addr，发一句话，返回对方的回答。连接被断开时回答是空的。
func tcpAsk(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		return ""
	}
	b, _ := io.ReadAll(c)
	return string(b)
}

func udpAsk(t *testing.T, c *net.UDPConn, msg string) string {
	t.Helper()
	buf := make([]byte, 2048)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := c.Read(buf)
		if err == nil {
			return string(buf[:n])
		}
	}
	return ""
}

func TestEntryEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A 是入口节点；"容器"在 B 上，顶替节点是 C。端口没有声明（testNodeAgent 的 Endpoint 没有 Ports），只靠入口的放行。
	ipB, ipC := netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")
	tcpPort := tcpEcho(t, ipB, 0, "B")
	tcpEcho(t, ipC, tcpPort, "C")
	otherPort := tcpEcho(t, ipB, 0, "secret")
	udpPort := taggedEcho(t, ipB, 0, "B")
	taggedEcho(t, ipC, udpPort, "C")

	A := newTestNodeAgent(t, ctx, log, netip.Addr{}, 0)
	B := newTestNodeAgent(t, ctx, log, ipB, 0)
	C := newTestNodeAgent(t, ctx, log, ipC, 0)
	B.alive.Store(true)
	connectMesh(t, A, B, C)

	nodes := []*nagiv1.LinkNode{linkNode(B), linkNode(C)}
	A.a.intranet.plan = &nagiv1.LinkPlan{Entries: []*nagiv1.LinkEntry{
		{ProjectId: testProject, Name: "web", Container: "dns", Service: "dns", Protocol: "tcp", Published: 18080, TargetName: "dns", TargetPort: uint32(tcpPort), Nodes: nodes},
		{ProjectId: testProject, Name: "game", Container: "dns", Service: "dns", Protocol: "udp", Published: 18081, TargetName: "dns", TargetPort: uint32(udpPort), Nodes: nodes},
	}}
	kA := A.m.PublicKey()
	allow := &nagiv1.LinkPlan{EntryAllows: []*nagiv1.LinkEntryAllow{
		{ProjectId: testProject, TargetName: "dns", Port: uint32(tcpPort), Protocol: "tcp", Callers: [][]byte{kA[:]}},
		{ProjectId: testProject, TargetName: "dns", Port: uint32(udpPort), Protocol: "udp", Callers: [][]byte{kA[:]}},
	}}
	B.a.intranet.plan, C.a.intranet.plan = allow, allow
	for _, n := range []*testNodeAgent{B, C} {
		go n.a.intranet.serve(ctx)
	}
	time.Sleep(200 * time.Millisecond)

	routes := A.a.intranet.entryRoutes()
	if len(routes) != 2 {
		t.Fatalf("入口规则: %+v", routes)
	}
	web, game := routeNamed(routes, "web"), routeNamed(routes, "game")
	if len(web.Upstreams) != 1 || !strings.HasPrefix(web.Upstreams[0], "127.0.0.1:") || web.Published != 18080 {
		t.Fatalf("web 规则不对: %+v", web)
	}

	// TCP 和 UDP 都转到 B 上的"容器"，端口没声明也能通。
	if got := tcpAsk(t, web.Upstreams[0], "hi"); got != "B:hi" {
		t.Fatalf("TCP 回答: %q", got)
	}
	uc, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.MustParseAddrPort(game.Upstreams[0])))
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if got := udpAsk(t, uc, "ping"); !strings.HasPrefix(got, "B:") || !strings.HasSuffix(got, ":ping") {
		t.Fatalf("UDP 回答: %q", got)
	}

	// 入口节点只能访问映射指向的端口：同一个容器的其他端口、TCP 放行不等于 UDP。
	if _, _, ok := A.a.intranet.dialRemote(B.m.PublicKey(), "B", entryProxy(A.a.intranet.plan.GetEntries()[0]), otherPort); ok {
		t.Fatal("入口节点不应该能访问别的端口")
	}
	if B.a.intranet.entryAllowed(testProject, "dns", tcpPort, "udp", kA) {
		t.Fatal("TCP 的放行不应该放行 UDP")
	}
	if kB := B.m.PublicKey(); B.a.intranet.entryAllowed(testProject, "dns", tcpPort, "tcp", kB) {
		t.Fatal("不在名单上的节点不应该放行")
	}

	// 顶替：容器到了 C，不用改计划就能连上。
	B.alive.Store(false)
	C.alive.Store(true)
	if got := tcpAsk(t, web.Upstreams[0], "hi"); got != "C:hi" {
		t.Fatalf("顶替后 TCP 回答: %q", got)
	}
	deadline := time.Now().Add(60 * time.Second)
	got := ""
	for time.Now().Before(deadline) && !strings.HasPrefix(got, "C:") {
		got = udpAsk(t, uc, "ping")
	}
	if !strings.HasPrefix(got, "C:") {
		t.Fatalf("顶替后 UDP 回答: %q", got)
	}

	// 放行被撤销：已经建立的连接也要断开（Caddy 会复用到上游的连接）。
	// tcpEcho 回完一句就关连接，这里用一个一直不回的"容器"。
	silent, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(ipC, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	silentPort := uint16(silent.Addr().(*net.TCPAddr).Port)
	C.a.intranet.mu.Lock()
	C.a.intranet.plan = &nagiv1.LinkPlan{EntryAllows: []*nagiv1.LinkEntryAllow{
		{ProjectId: testProject, TargetName: "dns", Port: uint32(silentPort), Protocol: "tcp", Callers: [][]byte{kA[:]}},
	}}
	C.a.intranet.mu.Unlock()
	mc, _, ok := A.a.intranet.dialRemote(C.m.PublicKey(), "C", entryProxy(A.a.intranet.plan.GetEntries()[0]), silentPort)
	if !ok {
		t.Fatal("放行的端口应该能连上")
	}
	defer mc.Close()
	C.a.intranet.mu.Lock()
	C.a.intranet.plan = &nagiv1.LinkPlan{}
	C.a.intranet.mu.Unlock()
	_ = mc.SetReadDeadline(time.Now().Add(3 * accessRecheck))
	if _, err := mc.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("撤销放行后连接应该被断开: %v", err)
	}

	// 项目放行被撤销（例如断开项目连接）：经项目放行建立的连接也断开，计划换掉时立刻检查。
	if err := C.a.intranet.set(&nagiv1.LinkPlan{Allows: []*nagiv1.LinkAllow{
		{ProjectId: testProject, Callers: [][]byte{kA[:]}, InternalPorts: []string{fmt.Sprintf("dns:%d", silentPort)}},
	}}); err != nil {
		t.Fatal(err)
	}
	mc2, _, ok := A.a.intranet.dialRemote(C.m.PublicKey(), "C", entryProxy(A.a.intranet.plan.GetEntries()[0]), silentPort)
	if !ok {
		t.Fatal("项目放行的端口应该能连上")
	}
	defer mc2.Close()
	if err := C.a.intranet.set(&nagiv1.LinkPlan{}); err != nil {
		t.Fatal(err)
	}
	_ = mc2.SetReadDeadline(time.Now().Add(accessRecheck / 2))
	if _, err := mc2.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("撤销项目放行后连接应该立刻断开: %v", err)
	}

	// 映射从计划里去掉：转发口关掉。
	A.a.intranet.plan = &nagiv1.LinkPlan{}
	if routes := A.a.intranet.entryRoutes(); len(routes) != 0 || len(A.a.intranet.entries) != 0 {
		t.Fatalf("去掉后还有: %+v", routes)
	}
	if _, err := net.DialTimeout("tcp", web.Upstreams[0], time.Second); err == nil {
		t.Fatal("转发口应该已经关掉")
	}
}

// 没有启动 Caddy 的节点不开转发口。
func TestEntryRoutesDisabledWithoutProxy(t *testing.T) {
	n := &intranet{a: &agent{cfg: Config{DisableProxy: true}}, entries: map[string]*entryForward{}, plan: &nagiv1.LinkPlan{Entries: []*nagiv1.LinkEntry{
		{ProjectId: testProject, Name: "web", Container: "dns", Service: "dns", Protocol: "tcp", Published: 18080, TargetName: "dns", TargetPort: 80},
	}}}
	if routes := n.entryRoutes(); len(routes) != 0 || len(n.entries) != 0 {
		t.Fatalf("不应该开: %+v", routes)
	}
}
