package mesh

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

type testNode struct {
	m    *Mesh
	port uint16
}

func newNode(t *testing.T, relay bool) testNode {
	t.Helper()
	var priv Key
	_, _ = rand.Read(priv[:])
	m, err := New(Config{PrivateKey: priv, Relay: relay, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return testNode{m: m, port: m.Port()}
}

func local(port uint16) []netip.AddrPort {
	return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)}
}

// setup 建一个面板 P 和两个节点 A、B：节点都能直连面板、互相也能直连，并且各有一条到面板的控制连接。
func setup(t *testing.T) (p, a, b testNode) { return setupWith(t, nil) }

// setupWith 同 setup，before 在加成员（也就是开始探测）之前调用，用来设置 dropUDP。
func setupWith(t *testing.T, before func(p, a, b testNode)) (p, a, b testNode) {
	p, a, b = newNode(t, true), newNode(t, false), newNode(t, false)
	if before != nil {
		before(p, a, b)
	}
	pk, ak, bk := p.m.PublicKey(), a.m.PublicKey(), b.m.PublicKey()
	_ = p.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}})
	_ = a.m.SetPeers([]Peer{{Key: pk, Endpoints: local(p.port)}, {Key: bk, Endpoints: local(b.port), Relays: []Key{pk}}})
	_ = b.m.SetPeers([]Peer{{Key: pk, Endpoints: local(p.port)}, {Key: ak, Endpoints: local(a.port), Relays: []Key{pk}}})
	for _, n := range []testNode{a, b} {
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
func echo(t *testing.T, a, b testNode) {
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
	defer func() { dropUDP = nil }()
	_, a, b := setupWith(t, func(_, a, b testNode) {
		dropUDP = func(from uint16, to netip.AddrPort) bool {
			return (from == a.port && to.Port() == b.port) || (from == b.port && to.Port() == a.port)
		}
	})
	waitPath(t, a.m, b.m.PublicKey(), "udp-relay")
	echo(t, a, b)
}

func TestTLSRelay(t *testing.T) {
	defer func() { dropUDP = nil }()
	_, a, b := setupWith(t, func(_, _, _ testNode) {
		dropUDP = func(uint16, netip.AddrPort) bool { return true }
	})
	waitPath(t, a.m, b.m.PublicKey(), "tls-relay")
	echo(t, a, b)
}

// TestUDP 经 mesh 收发 UDP：B 从随机端口连 A 开的端口。超过 MTU 的数据报靠 IPv6 分片。
func TestUDP(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	pc, err := a.m.ListenUDP(0)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if k, ok := a.m.KeyOf(from); !ok || k != b.m.PublicKey() {
				continue
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	c, err := b.m.DialUDP(a.m.PublicKey(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, size := range []int{5, MTU, 16 << 10} {
		msg := make([]byte, size)
		_, _ = rand.Read(msg)
		got := make([]byte, size+1)
		ok := false
		// UDP 可能丢包（例如 WireGuard 还在握手），多试几次。
		for try := 0; try < 10 && !ok; try++ {
			if _, err := c.Write(msg); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			n, err := c.Read(got)
			ok = err == nil && n == size && string(got[:n]) == string(msg)
		}
		if !ok {
			t.Fatalf("%d 字节的数据报没有原样回来", size)
		}
	}
}

// TestPeerRestart：对端重启（同一私钥、同一端口，会话全丢）后，新连接很快能建立，旧连接很快报错。
// 重启的一方加上成员时就发起握手，这时直连还没确认；要是这个握手包被丢掉，对方会一直用旧会话发包，
// 等 WireGuard 自己重新握手要十几秒。
func TestPeerRestart(t *testing.T) {
	dropUDP = nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var privB Key
	_, _ = rand.Read(privB[:])
	a := newNode(t, false)
	b, err := New(Config{PrivateKey: privB, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ka, kb := a.m.PublicKey(), b.PublicKey()
	portB := b.Port()
	_ = a.m.SetPeers([]Peer{{Key: kb, Endpoints: local(portB)}})
	_ = b.SetPeers([]Peer{{Key: ka, Endpoints: local(a.port)}})
	serve := func(m *Mesh) {
		ln, err := m.Listen(7000)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { _, _ = io.Copy(io.Discard, c) }()
			}
		}()
	}
	serve(b)
	waitPath(t, a.m, kb, "direct")
	old, err := a.m.Dial(context.Background(), kb, 7000)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	b.Close()
	time.Sleep(time.Second)
	b, err = New(Config{PrivateKey: privB, Port: portB, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_ = b.SetPeers([]Peer{{Key: ka, Endpoints: local(a.port)}})
	serve(b)
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := a.m.Dial(ctx, kb, 7000)
	if err != nil {
		t.Fatalf("对端重启后 5 秒内没能建立新连接：%v", err)
	}
	c.Close()
	t.Logf("对端重启后 %s 建立新连接", time.Since(start).Round(time.Millisecond))

	// 旧连接：对端已经没有这条连接了，写一下就会收到 RST。
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = old.SetDeadline(time.Now().Add(200 * time.Millisecond))
		_, werr := old.Write([]byte("x"))
		_, rerr := old.Read(make([]byte, 1))
		var ne net.Error
		if werr != nil || (rerr != nil && !(errors.As(rerr, &ne) && ne.Timeout())) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("对端重启后旧连接 5 秒内没有报错")
		}
	}
}

func TestAddress(t *testing.T) {
	var k Key
	_, _ = rand.Read(k[:])
	a := Address(k)
	if !Prefix.Contains(a) || a != Address(k) {
		t.Fatalf("地址不对：%s", a)
	}
}
