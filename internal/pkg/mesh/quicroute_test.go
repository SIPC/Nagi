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
			b.routedQUIC = &quicPacketConn{in: make(chan quicPacket, 1), done: make(chan struct{})}
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
			queue := b.routedQUIC
			b.handleControl(nil, 0, from, encodeRelay(typeRelayDeliver, peer, pkt))
			if queue != nil && len(queue.in) != 0 {
				t.Fatal("invalid routed packet delivered")
			}
			if len(b.quic.in) != 0 {
				t.Fatal("routed packet reached legacy transport")
			}
			// 正确包只进入逻辑地址空间，不把 R 写成 peer 的物理地址。
			r.nodeRelayEnabled = &on
			r.confirmed[from] = time.Now()
			b.routedQUIC = &quicPacketConn{in: make(chan quicPacket, 1), done: make(chan struct{})}
			payload = payload[:1200]
			b.handleControl(nil, 0, from, encodeRelay(typeRelayDeliver, peer, packet(b.pub, relay, peer)))
			select {
			case got := <-b.routedQUIC.in:
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
