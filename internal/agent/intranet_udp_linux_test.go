//go:build linux

package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 端到端测试 UDP 的整条路径（unixpacket 只有 Linux 有）：
// 代理容器那一侧（udpProxy）→ unixpacket → 节点 A → mesh 握手 → 节点 B/C → "容器"（本机 UDP 回显）。
// 只把找容器（FindEndpoint）换成假的，其余都是真的代码和真的 mesh。

const testProject = "p000000000000001"

// taggedEcho 在 ip 上开 UDP 回显，回"标记:来源端口:内容"。
func taggedEcho(t *testing.T, ip netip.Addr, port uint16, tag string) uint16 {
	t.Helper()
	pc, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDPAddrPort([]byte(fmt.Sprintf("%s:%d:%s", tag, from.Port(), buf[:n])), from)
		}
	}()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

type testNodeAgent struct {
	a     *agent
	m     *mesh.Mesh
	priv  mesh.Key
	alive atomic.Bool // 假容器是不是在本节点上运行
}

func newTestNodeAgent(t *testing.T, ctx context.Context, log *slog.Logger, target netip.Addr, port uint16) *testNodeAgent {
	t.Helper()
	var priv mesh.Key
	_, _ = rand.Read(priv[:])
	return startTestNodeAgent(t, ctx, log, priv, 0, target, port)
}

// startTestNodeAgent 用指定的私钥和 mesh 端口启动一个节点（模拟节点重启）。
func startTestNodeAgent(t *testing.T, ctx context.Context, log *slog.Logger, priv mesh.Key, meshPort uint16, target netip.Addr, port uint16) *testNodeAgent {
	t.Helper()
	m, err := mesh.New(mesh.Config{PrivateKey: priv, Port: meshPort, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	dir := t.TempDir()
	a := &agent{cfg: Config{DataDir: dir}, log: log, ctx: ctx, mesh: &meshNode{m: m}}
	tn := &testNodeAgent{a: a, m: m, priv: priv}
	n := &intranet{a: a, kick: make(chan struct{}, 1), file: filepath.Join(dir, "links.pb"),
		plan: &nagiv1.LinkPlan{}, socks: map[string]net.Listener{}, usocks: map[string]net.Listener{}}
	n.endpoint = func(_ context.Context, projectID, name string) (project.Endpoint, bool) {
		if projectID != testProject || name != "dns" || !tn.alive.Load() {
			return project.Endpoint{}, false
		}
		return project.Endpoint{IP: target, UDPPorts: []uint32{uint32(port)}}, true
	}
	a.intranet = n
	return tn
}

func TestUDPEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// B、C 上的"容器"：同一个端口号，不同的地址。
	ipB, ipC := netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")
	port := taggedEcho(t, ipB, 0, "B")
	taggedEcho(t, ipC, port, "C")

	A := newTestNodeAgent(t, ctx, log, netip.Addr{}, 0)
	B := newTestNodeAgent(t, ctx, log, ipB, port)
	C := newTestNodeAgent(t, ctx, log, ipC, port)
	B.alive.Store(true)

	all := []*testNodeAgent{A, B, C}
	for i, x := range all {
		var peers []mesh.Peer
		for j, y := range all {
			if i != j {
				peers = append(peers, mesh.Peer{Key: y.m.PublicKey(), Endpoints: []netip.AddrPort{
					netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), y.m.Port())}})
			}
		}
		if err := x.m.SetPeers(peers); err != nil {
			t.Fatal(err)
		}
	}
	waitDirect := func(from, to *testNodeAgent) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			for _, s := range from.m.Status() {
				if s.Key == to.m.PublicKey() && s.Path == "direct" {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("mesh 直连没有建立")
	}
	waitDirect(A, B)
	waitDirect(A, C)

	kA, kB, kC := A.m.PublicKey(), B.m.PublicKey(), C.m.PublicKey()
	// A：项目内网里放 dns 的代理，先找 B，再找 C（顶替节点）。
	A.a.intranet.plan = &nagiv1.LinkPlan{Proxies: []*nagiv1.LinkProxy{{
		ProjectId: testProject, Alias: "dns", TargetProject: testProject, TargetName: "dns",
		UdpPorts: []uint32{uint32(port)},
		Nodes:    []*nagiv1.LinkNode{{Key: kB[:], Name: "B"}, {Key: kC[:], Name: "C"}},
	}}}
	// B、C：允许 A 访问。
	for _, x := range []*testNodeAgent{B, C} {
		x.a.intranet.plan = &nagiv1.LinkPlan{Allows: []*nagiv1.LinkAllow{{ProjectId: testProject, Callers: [][]byte{kA[:]}}}}
		go x.a.intranet.serve(ctx)
	}
	A.a.intranet.syncSockets()
	sock := filepath.Join(A.a.intranet.sockDir(testProject), "udp")
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("A 没有开 UDP 的 socket：%v", err)
	}

	// 代理容器那一侧。
	ppc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ppc.Close()
	go newUDPProxy(ppc, sock, "dns", port).run()
	proxyAddr := ppc.LocalAddr().(*net.UDPAddr)

	newClient := func() *net.UDPConn {
		c, err := net.DialUDP("udp", nil, proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	// ask 一直发，直到收到 want 开头的回应（通道刚建、或者正在换节点时会丢包）。
	ask := func(c *net.UDPConn, msg, want string, within time.Duration) string {
		t.Helper()
		buf := make([]byte, 64<<10)
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if _, err := c.Write([]byte(msg)); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := c.Read(buf)
			if err == nil && strings.HasPrefix(string(buf[:n]), want) {
				return string(buf[:n])
			}
		}
		t.Fatalf("%s 内没有收到 %q 开头的回应", within, want)
		return ""
	}
	parse := func(r string) (tag, srcPort, body string) {
		parts := strings.SplitN(r, ":", 3)
		return parts[0], parts[1], parts[2]
	}

	// 1. 两个客户端经同一条通道到 B，各自是不同的流（容器看到不同的来源端口），回应不串。
	start := time.Now()
	c1, c2 := newClient(), newClient()
	r1 := ask(c1, "one", "B:", 15*time.Second)
	t.Logf("通道建立并收到第一个回应用了 %s", time.Since(start).Round(time.Millisecond))
	r2 := ask(c2, "two", "B:", 5*time.Second)
	r1b := ask(c1, "one-again", "B:", 5*time.Second)
	_, p1, b1 := parse(r1)
	_, p2, b2 := parse(r2)
	_, p1b, b1b := parse(r1b)
	if b1 != "one" || b2 != "two" || b1b != "one-again" {
		t.Fatalf("回应串了：%q %q %q", r1, r2, r1b)
	}
	if p1 != p1b || p1 == p2 {
		t.Fatalf("流的来源端口不对：c1=%s,%s c2=%s", p1, p1b, p2)
	}

	// 2. 复用已建好的通道：之后的请求不再握手，往返应该很快。
	t0 := time.Now()
	ask(c1, "fast", "B:", 2*time.Second)
	t.Logf("通道建好以后一次往返 %s", time.Since(t0).Round(time.Microsecond))

	// 2b. mesh 已经热了（WireGuard 会话已建好）时新建一条通道：另一个代理进程连进来，就是新的一次握手。
	ppc2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ppc2.Close()
	go newUDPProxy(ppc2, sock, "dns", port).run()
	c3, err := net.DialUDP("udp", nil, ppc2.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	time.Sleep(50 * time.Millisecond) // 等代理连上节点，只量通道的握手
	t0 = time.Now()
	ask(c3, "warm", "B:", 5*time.Second)
	t.Logf("mesh 热了以后新建通道到收到第一个回应 %s", time.Since(t0).Round(time.Microsecond))

	// 3. 大数据报（超过 mesh 的 MTU，靠 IPv6 分片）原样到达。
	big := strings.Repeat("x", 8<<10)
	if _, _, body := parse(ask(c2, big, "B:", 5*time.Second)); body != big {
		t.Fatalf("8 KiB 的数据报没有原样回来（%d 字节）", len(body))
	}
	// 超过上限的丢掉，不影响后面的。
	_, _ = c2.Write(make([]byte, udpMaxDatagram+1))
	ask(c2, "after-oversize", "B:", 5*time.Second)

	// 4. 未授权：D 没在 B 的允许名单里，握手被拒绝。
	D := newTestNodeAgent(t, ctx, log, netip.Addr{}, 0)
	kD := D.m.PublicKey()
	_ = D.m.SetPeers([]mesh.Peer{{Key: kB, Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), B.m.Port())}}})
	_ = B.m.SetPeers(append(peersOf(A, C), mesh.Peer{Key: kD, Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), D.m.Port())}}))
	waitDirect(D, B)
	px := A.a.intranet.plan.GetProxies()[0]
	if tun := D.a.intranet.dialRemoteUDP(kB, "B", px, port, func([]byte) {}); tun != nil {
		tun.close()
		t.Fatal("未授权的节点不应该建得起通道")
	}

	// 5. 顶替：容器从 B 挪到 C。B 的控制连接发现容器不在了就断开，A 重新握手，B 回"不在这里"，改连 C。
	B.alive.Store(false)
	C.alive.Store(true)
	start = time.Now()
	r := ask(c1, "after-failover", "C:", 40*time.Second)
	t.Logf("顶替后 %s 切到 C：%q", time.Since(start).Round(time.Millisecond), r)

	// 6. 目标节点重启（例如升级）：C 的进程停掉、用同一私钥和端口重新启动，mesh 会话和通道都丢了。
	// A 一直在发：mesh 很快重新握手，顺手写控制连接时收到 RST，马上重建通道。
	cPort := C.m.Port()
	C.m.Close()
	time.Sleep(2 * time.Second)
	C2 := startTestNodeAgent(t, ctx, log, C.priv, cPort, ipC, port)
	C2.alive.Store(true)
	C2.a.intranet.plan = &nagiv1.LinkPlan{Allows: []*nagiv1.LinkAllow{{ProjectId: testProject, Callers: [][]byte{kA[:]}}}}
	if err := C2.m.SetPeers(peersOf(A, B)); err != nil {
		t.Fatal(err)
	}
	go C2.a.intranet.serve(ctx)
	start = time.Now()
	r = ask(c1, "after-restart", "C:", 30*time.Second)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("目标节点重启后 %s 才恢复，太慢", d.Round(time.Millisecond))
	}
	t.Logf("目标节点重启后 %s 恢复：%q", time.Since(start).Round(time.Millisecond), r)
}

func peersOf(xs ...*testNodeAgent) []mesh.Peer {
	var out []mesh.Peer
	for _, x := range xs {
		out = append(out, mesh.Peer{Key: x.m.PublicKey(), Endpoints: []netip.AddrPort{
			netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), x.m.Port())}})
	}
	return out
}
