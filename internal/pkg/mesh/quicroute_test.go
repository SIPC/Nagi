package mesh

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestQUICRoutedStream(t *testing.T) {
	t.Parallel()
	a := newNodeConfig(t, Config{ExperimentalQUICRouting: true})
	b := newNodeConfig(t, Config{ExperimentalQUICRouting: true})
	r := newNodeConfig(t, Config{ExperimentalQUICRouting: true, NodeRelay: true, NoQUIC: true})
	ak, bk, rk := a.m.PublicKey(), b.m.PublicKey(), r.m.PublicKey()
	on, off := true, false
	set := func(n, other testNode, candidates []Key, permission *bool) {
		t.Helper()
		if err := n.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: permission},
			{Key: other.m.PublicKey(), Endpoints: local(other.port), NodeRelays: candidates}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}}); err != nil {
		t.Fatal(err)
	}
	set(a, b, nil, &on)
	set(b, a, nil, &on)
	for _, pair := range []struct{ from, to testNode }{{a, b}, {b, a}, {a, r}, {b, r}, {r, a}, {r, b}} {
		waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
	}
	set(a, b, []Key{rk}, &on)
	set(b, a, []Key{rk}, &on)
	c := quicEchoConnection(t, a, b)
	data := make([]byte, 256<<10)
	_, _ = rand.Read(data)
	exchange := func() {
		t.Helper()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		for range 4 {
			if _, err := c.Write(data); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(data))
			if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("original QUIC stream corrupted/interrupted: %v", err)
			}
		}
	}
	exchange()
	beforeRx, beforeTx := r.m.RelayBytes()
	if err := a.m.SetQUICRoute(bk, rk); err != nil {
		t.Fatal(err)
	}
	exchange() // A 经 R，B 直回，不要求两端同时切换。
	if err := b.m.SetQUICRoute(ak, rk); err != nil {
		t.Fatal(err)
	}
	var blockDirect atomic.Bool
	dropFor(t, func(from uint16, to netip.AddrPort) bool {
		return blockDirect.Load() && ((from == a.port && to.Port() == b.port) || (from == b.port && to.Port() == a.port))
	})
	blockDirect.Store(true)
	exchange() // 双向经 R，直连实际丢弃，不准偷走原直连。
	rx, tx := r.m.RelayBytes()
	if rx <= beforeRx+(2<<20) || tx <= beforeTx+(2<<20) {
		t.Fatalf("real relay traffic missing: rx=%d tx=%d before=%d/%d", rx, tx, beforeRx, beforeTx)
	}
	set(a, b, nil, &on)
	set(b, a, nil, &on)
	if s := a.m.ProbePath(context.Background(), bk, rk, 1253); s.Err != nil {
		t.Fatalf("selected QUIC route must remain observable after ranking removal: %+v", s)
	}
	exchange() // 排名移除不撤销手动选择且仍有独立许可的实验路径。
	blockDirect.Store(false)
	if err := a.m.SetQUICRoute(bk, Key{}); err != nil {
		t.Fatal(err)
	}
	exchange() // 显式恢复直连不要求对方同时恢复。
	if err := a.m.SetQUICRoute(bk, rk); !errors.Is(err, ErrQUICRoute) {
		t.Fatal("released route outside candidate ranking was resurrected")
	}
	set(a, b, []Key{rk}, &on)
	if err := a.m.SetQUICRoute(bk, rk); err != nil {
		t.Fatal(err)
	}
	set(a, b, []Key{rk}, &off)
	set(b, a, []Key{rk}, &off)
	r.m.SetNodeRelay(false)
	exchange() // 明确撤权恢复默认直连，仍是原流，并非关闭/重拨。
	if err := a.m.SetQUICRoute(bk, rk); !errors.Is(err, ErrQUICRoute) {
		t.Fatal("revoked relay accepted")
	}
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(c); err != nil || len(rest) != 0 {
		t.Fatalf("half-close after route changes: bytes=%d err=%v", len(rest), err)
	}
}

// TestQUICRouteRelayCannotHijack：授权中转 R 截下 A→B 的实验 QUIC 密文，改用自己和 B 的合法 MAC 重新封装，
// 并把 B 发往 R 逻辑地址的包同样转给 A。共用一个实验 Transport 时 B 按连接 ID 认下这些包，服务端连接迁到 R 的
// 逻辑地址：原流中断，A 恢复直连和 B 手动改路都救不回来。R 本来就能丢包，所以只要求它停手后原流恢复、身份不变。
func TestQUICRouteRelayCannotHijack(t *testing.T) {
	t.Parallel() // 只有这个测试用 tapUDP，而且只改写本测试端口之间的包。
	a := newNodeConfig(t, Config{ExperimentalQUICRouting: true})
	b := newNodeConfig(t, Config{ExperimentalQUICRouting: true})
	if bytes.Compare(a.m.self[:], b.m.self[:]) > 0 {
		a, b = b, a // 公钥小的一方拨号，让 B 是会跟随来源迁移的服务端。
	}
	r := newNodeConfig(t, Config{ExperimentalQUICRouting: true, NodeRelay: true, NoQUIC: true})
	ak, bk, rk := a.m.PublicKey(), b.m.PublicKey(), r.m.PublicKey()
	on := true
	set := func(n, other testNode, candidates []Key) {
		t.Helper()
		if err := n.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: &on},
			{Key: other.m.PublicKey(), Endpoints: local(other.port), NodeRelays: candidates}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}}); err != nil {
		t.Fatal(err)
	}
	set(a, b, nil)
	set(b, a, nil)
	for _, pair := range []struct{ from, to testNode }{{a, b}, {b, a}, {a, r}, {b, r}, {r, a}, {r, b}} {
		waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
	}
	set(a, b, []Key{rk})
	set(b, a, []Key{rk})
	c := quicEchoConnection(t, a, b)
	if err := a.m.SetQUICRoute(bk, rk); err != nil {
		t.Fatal(err)
	}
	r.m.bind.mu.Lock()
	macB, macA := r.m.bind.peers[bk].mac, r.m.bind.peers[ak].mac
	toA, sockA := r.m.bind.peers[ak].direct, r.m.bind.sock(r.m.bind.peers[ak].directConn)
	r.m.bind.mu.Unlock()
	rewrap := func(pkt []byte, target Key, mac [32]byte) []byte {
		out := append(header(typeQUICRoute, rk), pkt...)
		return append(out, quicRouteMAC(mac, out, target, Key{})...)
	}
	var rewrapped atomic.Int64
	tap := func(c *net.UDPConn, pkt []byte, to netip.AddrPort) bool {
		from := uint16(c.LocalAddr().(*net.UDPAddr).Port)
		m, ok := parse(pkt)
		switch {
		case !ok:
			return false
		case from == r.port && to.Port() == b.port && m.typ == typeRelayDeliver && m.key == ak:
			if inner, ok := parse(m.payload); ok && inner.typ == typeQUICRoute {
				_, _ = c.WriteToUDPAddrPort(rewrap(inner.payload, bk, macB), to)
				rewrapped.Add(1)
				return true
			}
		case from == b.port && to.Port() == r.port && m.typ == typeQUICRoute && m.key == bk:
			_, _ = sockA.WriteToUDPAddrPort(rewrap(m.payload, ak, macA), toA)
			return true
		}
		return false
	}
	tapUDP.Store(&tap)
	t.Cleanup(func() { tapUDP.Store(nil) })
	sent := []byte("written while relay rewraps;")
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(sent); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); rewrapped.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("relay never rewrapped a packet")
		}
	}
	time.Sleep(2 * time.Second) // 给 B 处理改封包、发路径挑战和 A 重传的时间。
	tapUDP.Store(nil)
	if err := a.m.SetQUICRoute(bk, Key{}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetQUICRoute(ak, Key{}); err != nil {
		t.Fatalf("B's connection to A no longer uses A's logical address: %v", err)
	}
	after := []byte("after relay stopped")
	if _, err := c.Write(after); err != nil {
		t.Fatal(err)
	}
	want := append(sent, after...)
	got := make([]byte, len(want))
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("original stream did not recover after relay stopped: %q %v", got, err)
	}
	b.m.quic.mu.Lock()
	defer b.m.quic.mu.Unlock()
	for conn, k := range b.m.quic.all {
		if k == ak && conn.RemoteAddr().String() != quicLogicalAddr(ak).String() {
			t.Fatalf("B's connection migrated to %s", conn.RemoteAddr())
		}
	}
}

func TestQUICRouteRequiresExplicitOptIn(t *testing.T) {
	for _, missing := range []string{"local", "peer", "relay", "permission", "candidate"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			a := newNodeConfig(t, Config{ExperimentalQUICRouting: missing != "local"})
			b := newNodeConfig(t, Config{ExperimentalQUICRouting: missing != "peer"})
			r := newNodeConfig(t, Config{ExperimentalQUICRouting: missing != "relay", NodeRelay: true, NoQUIC: true})
			ak, bk, rk := a.m.PublicKey(), b.m.PublicKey(), r.m.PublicKey()
			permission := missing != "permission"
			candidates := []Key{rk}
			if missing == "candidate" {
				candidates = nil
			}
			_ = r.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}})
			_ = a.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: &permission},
				{Key: bk, Endpoints: local(b.port), NodeRelays: candidates}})
			_ = b.m.SetPeers([]Peer{{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: &permission},
				{Key: ak, Endpoints: local(a.port), NodeRelays: candidates}})
			for _, pair := range []struct{ from, to testNode }{{a, b}, {b, a}, {a, r}, {b, r}, {r, a}, {r, b}} {
				waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
			}
			c := quicEchoConnection(t, a, b)
			if err := a.m.SetQUICRoute(bk, rk); !errors.Is(err, ErrQUICRoute) {
				t.Fatalf("missing %s was not rejected: %v", missing, err)
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write([]byte("keep existing connection")); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 24)
			if _, err := io.ReadFull(c, got); err != nil || string(got) != "keep existing connection" {
				t.Fatalf("rejection broke existing connection: %q %v", got, err)
			}
		})
	}
}

func TestQUICRouteAuthenticatedDemux(t *testing.T) {
	for _, attack := range []string{"mac", "target", "relay", "source", "unconfirmed", "expired", "revoked", "disabled", "oversized"} {
		t.Run(attack, func(t *testing.T) {
			b := newBind(Key{8}, false, capQUIC|capQUICRoute, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			peer, relay := PublicKey(Key{16}), PublicKey(Key{24})
			on := true
			b.setPeers([]Peer{{Key: peer}, {Key: relay, NodeRelayEnabled: &on}})
			p, r := b.peers[peer], b.peers[relay]
			from := netip.MustParseAddrPort("127.0.0.1:12345")
			r.direct, r.directAt, r.caps, r.capsAt = from, time.Now(), capQUICRoute, time.Now()
			r.confirmed[from], b.byAddr[from] = time.Now(), relay
			// 每个发送方各自的队列：别人的合法封装不能进入 peer 的 Transport。
			queues := map[Key]*quicPacketConn{}
			routed := func(k Key) *quicPacketConn {
				if queues[k] == nil {
					queues[k] = &quicPacketConn{in: make(chan quicPacket, 1), done: make(chan struct{})}
				}
				return queues[k]
			}
			b.routedQUIC = routed
			b.quic = &quicPacketConn{in: make(chan quicPacket, 1), done: make(chan struct{})}
			payload := append([]byte{0x40}, bytes.Repeat([]byte{0x71}, 1199)...)
			packet := func(target, via, source Key) []byte {
				pkt := append(header(typeQUICRoute, source), payload...)
				return append(pkt, quicRouteMAC(p.mac, pkt, target, via)...)
			}
			pkt := packet(b.pub, relay, peer)
			switch attack {
			case "mac":
				pkt[len(pkt)-1] ^= 1
			case "target":
				pkt = packet(PublicKey(Key{32}), relay, peer)
			case "relay":
				pkt = packet(b.pub, PublicKey(Key{32}), peer)
			case "source":
				pkt = packet(b.pub, relay, relay)
			case "unconfirmed":
				delete(r.confirmed, from)
			case "expired":
				r.confirmed[from] = time.Now().Add(-directTimeout)
			case "revoked":
				off := false
				r.nodeRelayEnabled = &off
			case "disabled":
				b.routedQUIC = nil
			case "oversized":
				payload = append(payload, 1)
				pkt = packet(b.pub, relay, peer)
			}
			b.handleControl(nil, 0, from, encodeRelay(typeRelayDeliver, peer, pkt))
			for k, q := range queues {
				if len(q.in) != 0 {
					t.Fatalf("invalid routed packet delivered to %s", k.Short())
				}
			}
			if len(b.quic.in) != 0 {
				t.Fatal("routed packet reached legacy transport")
			}
			// 正确包只进入逻辑地址空间，不把 R 写成 peer 的物理地址。
			r.nodeRelayEnabled = &on
			r.confirmed[from] = time.Now()
			b.routedQUIC = routed
			payload = payload[:1200]
			b.handleControl(nil, 0, from, encodeRelay(typeRelayDeliver, peer, packet(b.pub, relay, peer)))
			if q := queues[relay]; q != nil && len(q.in) != 0 {
				t.Fatal("peer's packet reached the relay's own transport")
			}
			select {
			case got := <-routed(peer).in:
				if got.from != quicLogicalAddr(peer) || !bytes.Equal(got.data, payload) || p.direct.IsValid() {
					t.Fatal("wrong logical source, payload or physical adjacency")
				}
			default:
				t.Fatal("valid authenticated packet rejected")
			}
		})
	}
}

func quicEchoConnection(t *testing.T, a, b testNode) net.Conn {
	t.Helper()
	ln, err := b.m.Listen(7150)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
		if c != nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if warm, err := a.m.quic.open(ctx, b.m.PublicKey(), 7199); err == nil {
		_ = warm.Close()
	}
	waitQUIC(t, a.m, b.m.PublicKey())
	c, err := a.m.Dial(ctx, b.m.PublicKey(), 7150)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, ok := c.(*quicStream); !ok {
		t.Fatalf("must exercise QUIC, got %T", c)
	}
	select {
	case server := <-accepted:
		if server == nil {
			t.Fatal("server did not accept QUIC stream")
		}
		if k, ok := b.m.KeyOf(server.RemoteAddr()); !ok || k != a.m.PublicKey() {
			t.Fatal("server did not authenticate original caller")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return c
}
