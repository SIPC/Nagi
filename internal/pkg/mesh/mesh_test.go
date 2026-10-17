package mesh

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"
)

type member struct {
	m    *Mesh
	port uint16
}

func newMember(t *testing.T, relay bool) member {
	t.Helper()
	var priv Key
	_, _ = rand.Read(priv[:])
	m, err := New(Config{PrivateKey: priv, Relay: relay, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return member{m: m, port: m.Port()}
}

func local(port uint16) []netip.AddrPort {
	return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)}
}

// setup 建一个面板 P 和两个节点 A、B：节点都能直连面板、互相也能直连，并且各有一条到面板的控制连接。
func setup(t *testing.T) (p, a, b member) {
	p, a, b = newMember(t, true), newMember(t, false), newMember(t, false)
	pk, ak, bk := p.m.PublicKey(), a.m.PublicKey(), b.m.PublicKey()
	_ = p.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}})
	_ = a.m.SetPeers([]Peer{{Key: pk, Endpoints: local(p.port)}, {Key: bk, Endpoints: local(b.port), Relays: []Key{pk}}})
	_ = b.m.SetPeers([]Peer{{Key: pk, Endpoints: local(p.port)}, {Key: ak, Endpoints: local(a.port), Relays: []Key{pk}}})
	for _, n := range []member{a, b} {
		node := n
		// 节点 → 面板：面板按连接确认发送方，不信任包里写的来源。
		node.m.AttachLink(pk, func(_, dst Key, payload []byte) error {
			go p.m.HandleLink(node.m.PublicKey(), dst, payload)
			return nil
		})
		p.m.AttachLink(node.m.PublicKey(), func(src, dst Key, payload []byte) error {
			go node.m.HandleLink(src, dst, payload)
			return nil
		})
	}
	return p, a, b
}

// echo 在 B 上监听，从 A 经 mesh 连过去，确认能收发。
func echo(t *testing.T, a, b member) {
	t.Helper()
	ln, err := b.m.Listen(7000)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := a.m.Dial(ctx, b.m.PublicKey(), 7000)
	if err != nil {
		t.Fatalf("经 mesh 连接失败：%v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("回显不对：%q %v", buf, err)
	}
	if k, ok := b.m.KeyOf(c.LocalAddr()); ok {
		_ = k
	}
}

func pathTo(m *Mesh, k Key) string {
	for _, s := range m.Status() {
		if s.Key == k {
			return s.Path
		}
	}
	return "missing"
}

func waitPath(t *testing.T, m *Mesh, k Key, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if pathTo(m, k) == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("路径应该是 %s，实际 %s", want, pathTo(m, k))
}

func TestDirect(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	echo(t, a, b)
}

func TestUDPRelay(t *testing.T) {
	_, a, b := setup(t)
	dropUDP = func(from uint16, to netip.AddrPort) bool {
		return (from == a.port && to.Port() == b.port) || (from == b.port && to.Port() == a.port)
	}
	defer func() { dropUDP = nil }()
	waitPath(t, a.m, b.m.PublicKey(), "udp-relay")
	echo(t, a, b)
}

func TestTLSRelay(t *testing.T) {
	_, a, b := setup(t)
	dropUDP = func(uint16, netip.AddrPort) bool { return true }
	defer func() { dropUDP = nil }()
	waitPath(t, a.m, b.m.PublicKey(), "tls-relay")
	echo(t, a, b)
}

func TestAddress(t *testing.T) {
	var k Key
	_, _ = rand.Read(k[:])
	a := Address(k)
	if !Prefix.Contains(a) || a != Address(k) {
		t.Fatalf("地址不对：%s", a)
	}
}
