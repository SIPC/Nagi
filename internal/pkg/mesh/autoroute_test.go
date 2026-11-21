package mesh

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAutoRouteSwitchAndFallBack 用真实的三个节点：A、B 直连，R 在中转池里。比较结果换成可控的假建议，
// 测执行层本身：连续两个窗口才切、切换时同一条 TCP 连接不断、新连接跟着走中转而不走 QUIC、
// 已有的 QUIC 连接留在直连、R 中途断了经健康检查几秒内回到直连并暂停用它、关掉开关立即回到直连。
func TestAutoRouteSwitchAndFallBack(t *testing.T) {
	t.Parallel()
	a, b := newNodeConfig(t, Config{}), newNodeConfig(t, Config{})
	r := newNodeConfig(t, Config{NodeRelay: true, NoQUIC: true})
	ak, bk, rk := a.m.PublicKey(), b.m.PublicKey(), r.m.PublicKey()
	on := true
	_ = r.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}})
	_ = a.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: &on, RelayPool: true}, {Key: bk, Endpoints: local(b.port)}})
	_ = b.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: &on, RelayPool: true}, {Key: ak, Endpoints: local(a.port)}})
	for _, pair := range []struct{ from, to testNode }{{a, b}, {b, a}, {a, r}, {b, r}, {r, a}, {r, b}} {
		waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
	}

	// 已有的 QUIC 连接：切换前建好，切换后仍然能用、仍然是直连的那条（不迁移、不断）。
	quicConn := quicEchoConnection(t, a, b)
	// 隧道里的 TCP 连接：经 WireGuard，切换时对端公钥不变，同一条连接接着用。
	ln, err := b.m.Listen(7300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wg, err := a.m.dialWireGuard(ctx, bk, 7300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wg.Close() })
	roundTrip := func(c net.Conn, what string) {
		t.Helper()
		msg := []byte("ping-" + what)
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("%s write: %v", what, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != string(msg) {
			t.Fatalf("%s echo: %q %v", what, buf, err)
		}
	}
	roundTrip(wg, "before")

	// 假的比较：每个窗口都建议经 R。
	var mu sync.Mutex
	advise, windows := rk, 0
	ar := &autoRoute{gap: 50 * time.Millisecond, busy: 0, health: 100 * time.Millisecond}
	ar.observe = func(_ context.Context, peer Key, relays []Key) PathAdvice {
		mu.Lock()
		defer mu.Unlock()
		if peer != bk {
			return PathAdvice{}
		}
		windows++
		return PathAdvice{Sufficient: true, Via: advise, Paths: []PathReport{{}, {Via: rk}}}
	}
	a.m.setAutoRoute(true, ar)
	waitPath(t, a.m, bk, "node-relay")
	mu.Lock()
	if windows < autoStreak {
		t.Fatalf("switched after %d windows, want at least %d", windows, autoStreak)
	}
	mu.Unlock()
	if s := statusOf(a.m, bk); s.Via != rk || !s.PhysicalDirect.IsValid() {
		t.Fatalf("relay status: %+v", s)
	}
	// 同一条连接接着用，而且真的经 R：R 的转发计数在涨。
	before, _ := r.m.RelayBytes()
	roundTrip(wg, "relayed")
	if after, _ := r.m.RelayBytes(); after == before {
		t.Fatal("traffic did not go through R")
	}
	roundTrip(quicConn, "existing-quic")
	// 新连接走 WireGuard（跟着走中转），不是 QUIC。
	fresh, err := a.m.Dial(ctx, bk, 7300)
	if err != nil {
		t.Fatal(err)
	}
	if _, isQUIC := fresh.(*quicStream); isQUIC {
		t.Fatal("new connection used QUIC while routed through R")
	}
	roundTrip(fresh, "fresh")
	_ = fresh.Close()

	// 双发：直连 A → B 断了（R 还通），同一条连接照样马上收到回显，经 R 的那份送到了。
	cutDirect := atomic.Bool{}
	dropFor(t, func(from uint16, to netip.AddrPort) bool {
		return cutDirect.Load() && from == a.port && to.Port() == b.port
	})
	cutDirect.Store(true)
	quick := func(what string) {
		t.Helper()
		start := time.Now()
		roundTrip(wg, what)
		// 健康检查每次探测最多等 800 ms、连续 3 次才回退；只发一条路时要等它回退再等 TCP 重传，远超这个时间。
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("%s took %v", what, d)
		}
	}
	quick("direct-cut")
	cutDirect.Store(false)

	// R 不再转发给 B（例如 R 和 B 之间断了）：双发的直连那份马上送到，不等回退；
	// 健康检查连续失败后回到直连，之后不再选 R。
	r.m.SetNodeRelay(false)
	start := time.Now()
	quick("relay-cut")
	if p := pathTo(a.m, bk); p != "node-relay" {
		t.Fatalf("fell back before the relay-cut echo was checked: %s", p)
	}
	waitPath(t, a.m, bk, "direct")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("fallback took %v", d)
	}
	roundTrip(wg, "fallback")
	r.m.SetNodeRelay(true)
	time.Sleep(500 * time.Millisecond) // 好几个窗口都建议 R，但 R 暂停使用中
	if p := pathTo(a.m, bk); p != "direct" {
		t.Fatalf("banned relay reused: %s", p)
	}

	// 关掉开关：所有性能路线立即恢复直连。
	a.m.bind.mu.Lock()
	a.m.bind.peers[bk].perfVia = rk
	a.m.bind.mu.Unlock()
	if p := pathTo(a.m, bk); p != "node-relay" {
		t.Fatalf("setup: %s", p)
	}
	a.m.SetAutoRoute(false)
	if p := pathTo(a.m, bk); p != "direct" || a.m.AutoRoute() {
		t.Fatalf("switch off kept relay: %s", p)
	}
	roundTrip(wg, "off")
}

func statusOf(m *Mesh, k Key) Status {
	for _, s := range m.Status() {
		if s.Key == k {
			return s
		}
	}
	return Status{}
}

// TestAutoRouteApply 测切换规则：期望值按 docs/mesh.md#自动切路 的规则推出来。
func TestAutoRouteApply(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := newBind(Key{8}, false, 0, 0, log)
	peer, r1, r2 := PublicKey(Key{16}), PublicKey(Key{24}), PublicKey(Key{32})
	on := true
	b.setPeers([]Peer{{Key: peer}, {Key: r1, NodeRelayEnabled: &on, RelayPool: true}, {Key: r2, NodeRelayEnabled: &on, RelayPool: true}})
	m := &Mesh{bind: b, log: log}
	a := &autoRoute{m: m, state: map[Key]*routeState{}, heavy: map[Key]bool{}}
	t0 := time.Now()
	advise := func(via Key) PathAdvice { return PathAdvice{Sufficient: true, Via: via} }
	cur := func() Key { return b.peers[peer].perfVia }
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	a.apply(peer, advise(r1), at(0))
	if cur() != (Key{}) {
		t.Fatal("switched after one window")
	}
	a.apply(peer, PathAdvice{}, at(4)) // 证据不足：连续计数清零，路线不动
	a.apply(peer, advise(r1), at(8))
	if cur() != (Key{}) {
		t.Fatal("insufficient window did not reset the streak")
	}
	a.apply(peer, advise(r1), at(12))
	if cur() != r1 {
		t.Fatal("two consistent windows did not switch")
	}
	// 试用期：下一个窗口建议别的（这里是 r2），马上回直连，r1 暂停使用。
	a.apply(peer, advise(r2), at(16))
	if cur() != (Key{}) {
		t.Fatal("failed trial kept the relay")
	}
	// 被暂停的 r1 再被建议也当成直连；冷却期内 r2 连续两次也不切。
	a.apply(peer, advise(r1), at(20))
	a.apply(peer, advise(r2), at(20))
	a.apply(peer, advise(r2), at(24))
	if cur() != (Key{}) {
		t.Fatal("switched during cooldown")
	}
	// 冷却（10 分钟）过了：r2 连续两次就切，再一个窗口确认后试用结束。
	a.apply(peer, advise(r2), at(28))
	a.apply(peer, advise(r2), at(32))
	if cur() != r2 {
		t.Fatalf("did not switch after cooldown: %v", cur())
	}
	a.apply(peer, advise(r2), at(36))
	// 试用结束后，一个窗口建议直连不回，连续两个才回（直连不受冷却限制）。
	a.apply(peer, advise(Key{}), at(40))
	if cur() != r2 {
		t.Fatal("one direct window flapped back")
	}
	a.apply(peer, advise(Key{}), at(41))
	if cur() != (Key{}) {
		t.Fatal("two direct windows did not switch back")
	}
	// 健康检查：两次失败不回，第三次回；中间成功一次就重新计数。
	b.peers[peer].perfVia = r2
	fail := []healthResult{{peer: peer, via: r2, failed: true}}
	a.healthDone(fail, at(50))
	a.healthDone(fail, at(50))
	a.healthDone([]healthResult{{peer: peer, via: r2}}, at(50))
	a.healthDone(fail, at(50))
	a.healthDone(fail, at(50))
	if cur() != r2 {
		t.Fatal("non-consecutive failures fell back")
	}
	a.healthDone(fail, at(50))
	if cur() != (Key{}) {
		t.Fatal("three consecutive failures did not fall back")
	}
	// 流量超过中转限速一半的节点对不切到中转。
	a.state = map[Key]*routeState{}
	a.heavy[peer] = true
	a.apply(peer, advise(r1), at(100))
	a.apply(peer, advise(r1), at(104))
	if cur() != (Key{}) {
		t.Fatal("heavy pair moved onto a rate-limited relay")
	}
	// 退出中转池：发包时立即不用它，setPeers 也清掉。
	a.heavy[peer] = false
	a.apply(peer, advise(r1), at(108))
	if cur() != r1 {
		t.Fatal("setup")
	}
	b.setPeers([]Peer{{Key: peer}, {Key: r1, NodeRelayEnabled: &on}, {Key: r2, NodeRelayEnabled: &on, RelayPool: true}})
	if cur() != (Key{}) {
		t.Fatal("leaving the pool kept the route")
	}
}
