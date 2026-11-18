//go:build meshbench && linux

package mesh

// 隧道里的 TCP 的实测（不在平时的测试里跑）：同一个进程里两个 mesh 成员经 127.0.0.1 直连，
// 线路的延迟、丢包、限速用 tc netem 加在 lo 上，例如：
//
//	tc qdisc add dev lo root netem delay 20ms loss 1% rate 100mbit
//	MESH_BENCH_MB=100 go test -tags meshbench -run MeshBench -v ./internal/pkg/mesh/
//
// lo 上两个方向共用一个队列，和真实线路差得多。MESH_BENCH_NETNS=<名字> 时 B 放在这个网络命名空间里，
// 两边经一对 veth（A 10.9.0.1，B 10.9.0.2）相连，各自出口加 netem，例如：
//
//	ip netns add nb && ip link add va type veth peer name vb && ip link set vb netns nb
//	ip addr add 10.9.0.1/24 dev va && ip link set va up
//	ip netns exec nb sh -c 'ip addr add 10.9.0.2/24 dev vb && ip link set vb up && ip link set lo up'
//	tc qdisc add dev va root netem delay 20ms loss 1% rate 100mbit
//	ip netns exec nb tc qdisc add dev vb root netem delay 20ms loss 1%
//
// 环境变量：MESH_BENCH_STREAMS（并行连接数，默认 4）、MESH_BENCH_QUIC=1（走 QUIC）、
// MESH_BENCH_TUNING=gvisor（用 gVisor 的默认参数对比），MESH_BENCH_CC、MESH_BENCH_BUF（MiB）、MESH_BENCH_MOD 单独改某一项。

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
)

func benchEnv(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func benchTuning() tcpTuning {
	t := defaultTuning
	if os.Getenv("MESH_BENCH_TUNING") == "gvisor" {
		t = gvisorDefaults
	}
	if cc := os.Getenv("MESH_BENCH_CC"); cc != "" {
		t.cc = cc
	}
	if b := benchEnv("MESH_BENCH_BUF", 0); b > 0 {
		t.sendMax, t.recvMax = b<<20, b<<20
	}
	if b := benchEnv("MESH_BENCH_DEF", 0); b > 0 {
		t.sendDefault, t.recvDefault = b<<20, b<<20
	}
	if v := os.Getenv("MESH_BENCH_RACK"); v != "" {
		t.noRACK = v == "0"
	}
	if v := os.Getenv("MESH_BENCH_MOD"); v != "" {
		t.moderate = v == "1"
	}
	return t
}

// benchPair 建两个直连的 mesh 成员。
func benchPair(t *testing.T, quic bool) (*Mesh, *Mesh) {
	t.Helper()
	defaultTuning = benchTuning()
	if v := benchEnv("MESH_BENCH_SOCKBUF", -1); v >= 0 {
		udpSocketBuffer = v << 10 // KiB
	}
	if os.Getenv("MESH_BENCH_TUNING") == "gvisor" {
		// 和改动以前一样：gVisor 的默认参数、一次交一个包、系统默认的 UDP 缓冲。
		tunBatch, udpSocketBuffer = false, 0
	}
	var w io.Writer = io.Discard
	if os.Getenv("MESH_BENCH_DEBUG") == "1" {
		w = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var pa, pb Key
	_, _ = rand.Read(pa[:])
	_, _ = rand.Read(pb[:])
	ma, err := New(Config{PrivateKey: pa, NoQUIC: !quic, NoFEC: benchEnv("MESH_BENCH_NOFEC", 0) == 1, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ma.Close)
	ipA, ipB := "127.0.0.1", "127.0.0.1"
	var mb *Mesh
	newB := func() {
		mb, err = New(Config{PrivateKey: pb, NoQUIC: !quic, NoFEC: benchEnv("MESH_BENCH_NOFEC", 0) == 1, Logger: log})
	}
	if ns := os.Getenv("MESH_BENCH_NETNS"); ns != "" {
		ipA, ipB = "10.9.0.1", "10.9.0.2"
		inNetns(t, ns, newB)
	} else {
		newB()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mb.Close)
	at := func(ip string, port uint16) []netip.AddrPort {
		return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr(ip), port)}
	}
	_ = ma.SetPeers([]Peer{{Key: mb.PublicKey(), Endpoints: at(ipB, mb.Port())}})
	_ = mb.SetPeers([]Peer{{Key: ma.PublicKey(), Endpoints: at(ipA, ma.Port())}})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if pathTo(ma, mb.PublicKey()) == "direct" && pathTo(mb, ma.PublicKey()) == "direct" && (!quic || quicTo(ma, mb.PublicKey())) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("没有建好直连")
		}
	}
	// 先连一次：WireGuard 握手、QUIC 连接都建好，下面量的只是传输。
	warm, err := mb.Listen(7199)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if c, err := warm.Accept(); err == nil {
			c.Close()
		}
	}()
	start := time.Now()
	if c, err := ma.Dial(context.Background(), mb.PublicKey(), 7199); err == nil {
		c.Close()
	}
	warm.Close()
	t.Logf("参数：%+v，UDP 缓冲 %d KiB，QUIC=%v，第一次连上用时 %v", defaultTuning, udpSocketBuffer>>10, quic, time.Since(start))
	return ma, mb
}

// inNetns 在网络命名空间 ns 里调用 fn：fn 里建的套接字留在那个命名空间（mesh 的 UDP 端口在 New 里打开）。
func inNetns(t *testing.T, ns string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread() // 不解锁：这个线程的命名空间换过了，goroutine 结束时线程随之退出
		self, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			t.Error(err)
			return
		}
		defer self.Close()
		target, err := os.Open("/var/run/netns/" + ns)
		if err != nil {
			t.Error(err)
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			t.Error(err)
			return
		}
		fn()
	}()
	<-done
}

// TestMeshBench：streams 条连接一起往对方发 MB MiB，对方丢掉，算吞吐。
func TestMeshBench(t *testing.T) {
	quic := benchEnv("MESH_BENCH_QUIC", 0) == 1
	streams := benchEnv("MESH_BENCH_STREAMS", 4)
	total := int64(benchEnv("MESH_BENCH_MB", 100)) << 20
	ma, mb := benchPair(t, quic)
	ln, err := mb.Listen(7100)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var got sync.WaitGroup
	var received atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			got.Add(1)
			go func() {
				defer got.Done()
				defer c.Close()
				_, _ = io.Copy(countWriter{&received}, c)
			}()
		}
	}()
	buf := make([]byte, 256<<10)
	_, _ = rand.Read(buf)
	start := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	if ms := benchEnv("MESH_BENCH_TRACE", 0); ms > 0 {
		go func() {
			var last int64
			var line []string
			for {
				select {
				case <-stop:
					t.Logf("每 %d ms 收到（KiB）：%s", ms, strings.Join(line, " "))
					return
				case <-time.After(time.Duration(ms) * time.Millisecond):
				}
				n := received.Load()
				line = append(line, strconv.FormatInt((n-last)>>10, 10))
				last = n
			}
		}()
	}
	if benchEnv("MESH_BENCH_INFO", 0) == 1 {
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Second):
				}
				for _, e := range ma.tnet.stack.RegisteredEndpoints() {
					if ep, ok := e.(tcpip.Endpoint); ok {
						var info tcpip.TCPInfoOption
						if ep.GetSockOpt(&info) == nil && info.SndCwnd > 0 && info.RTT > 0 {
							rb := ep.SocketOptions().GetReceiveBufferSize()
							sb := ep.SocketOptions().GetSendBufferSize()
							var cc tcpip.CongestionControlOption
							_ = ep.GetSockOpt(&cc)
							t.Logf("%s rtt %v cwnd %d ssthresh %d cc %d sndbuf %d rcvbuf %d queued %d/%d", cc, info.RTT, info.SndCwnd, info.SndSsthresh, info.CcState, sb, rb, ma.tnet.ep.NumQueued(), mb.tnet.ep.NumQueued())
						}
					}
				}
				for _, e := range mb.tnet.stack.RegisteredEndpoints() {
					if ep, ok := e.(tcpip.Endpoint); ok {
						rb := ep.SocketOptions().GetReceiveBufferSize()
						var info tcpip.TCPInfoOption
						if ep.GetSockOpt(&info) == nil && info.SndCwnd > 0 && info.RTT > 0 {
							t.Logf("  receiver rcvbuf %d", rb)
						}
					}
				}
			}
		}()
	}
	var wg sync.WaitGroup
	for i := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialStart := time.Now()
			c, err := ma.Dial(context.Background(), mb.PublicKey(), 7100)
			if err != nil {
				t.Error(err)
				return
			}
			if i == 0 {
				t.Logf("连上用时 %v", time.Since(dialStart))
			}
			left := total / int64(streams)
			if i == 0 {
				left += total % int64(streams)
			}
			for left > 0 {
				n := min(left, int64(len(buf)))
				if _, err := c.Write(buf[:n]); err != nil {
					t.Error(err)
					return
				}
				left -= n
			}
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			_, _ = io.Copy(io.Discard, c) // 等对方收完关掉
			c.Close()
		}()
	}
	wg.Wait()
	got.Wait()
	d := time.Since(start)
	t.Logf("结果：%d MiB，%d 条连接，QUIC=%v，用时 %.1f 秒，%.2f MiB/s", total>>20, streams, quic, d.Seconds(), float64(total)/(1<<20)/d.Seconds())
	st := ma.tnet.stack.Stats()
	t.Logf("发送端 TCP：发出 %d 段，重传 %d，超时 %d，快速重传 %d，快速恢复 %d，SACK 恢复 %d，TLP %d；IP 发出失败 %d，链路丢 %d",
		st.TCP.SegmentsSent.Value(), st.TCP.Retransmits.Value(), st.TCP.Timeouts.Value(), st.TCP.FastRetransmit.Value(),
		st.TCP.FastRecovery.Value(), st.TCP.SACKRecovery.Value(), st.TCP.TLPRecovery.Value(), st.IP.OutgoingPacketErrors.Value(), st.NICs.TxPacketsDroppedNoBufferSpace.Value())
}

// TestMeshBenchLatency：一条连接上来回 64 字节的小请求，算往返时间（容器跨节点访问、项目内网的典型用法）。
func TestMeshBenchLatency(t *testing.T) {
	quic := benchEnv("MESH_BENCH_QUIC", 0) == 1
	ma, mb := benchPair(t, quic)
	ln, err := mb.Listen(7101)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	c, err := ma.Dial(context.Background(), mb.PublicKey(), 7101)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := make([]byte, 64)
	var rtts []time.Duration
	for range benchEnv("MESH_BENCH_N", 200) {
		start := time.Now()
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, msg); err != nil {
			t.Fatal(err)
		}
		rtts = append(rtts, time.Since(start))
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	t.Logf("结果：小请求往返 p50 %.2f ms，p90 %.2f ms，p99 %.2f ms（QUIC=%v）", ms(rtts[len(rtts)/2]), ms(rtts[len(rtts)*9/10]), ms(rtts[len(rtts)*99/100]), quic)
}

type countWriter struct{ n *atomic.Int64 }

func (w countWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// TestMeshBenchMemory：开 N 条连接，空闲时和每条都在收发（每条发 2 MiB 不读、把缓冲塞满）时，每条连接多占多少内存。
func TestMeshBenchMemory(t *testing.T) {
	n := benchEnv("MESH_BENCH_CONNS", 200)
	ma, mb := benchPair(t, false)
	ln, err := mb.Listen(7102)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var server []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			server = append(server, c)
			mu.Unlock()
		}
	}()
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var s runtime.MemStats
		runtime.ReadMemStats(&s)
		return s.HeapInuse
	}
	base := heap()
	var conns []net.Conn
	for range n {
		c, err := ma.Dial(context.Background(), mb.PublicKey(), 7102)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(time.Second)
	idle := heap()
	// 每条连接发 2 MiB，对方不读：数据积在对方的接收缓冲和这边的发送缓冲里，直到窗口满。
	chunk := make([]byte, 2<<20)
	for _, c := range conns {
		go func() { _, _ = c.Write(chunk) }()
	}
	time.Sleep(5 * time.Second)
	full := heap()
	t.Logf("结果：%d 条连接，空闲每条 %.1f KiB，缓冲塞满时每条 %.1f KiB（参数 %+v）", n,
		float64(idle-base)/float64(n)/1024, float64(full-base)/float64(n)/1024, defaultTuning)
	for _, c := range conns {
		c.Close()
	}
	mu.Lock()
	for _, c := range server {
		c.Close()
	}
	mu.Unlock()
	_ = fmt.Sprint()
}
