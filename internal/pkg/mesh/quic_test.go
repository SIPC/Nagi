package mesh

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func quicTo(m *Mesh, k Key) bool {
	for _, s := range m.Status() {
		if s.Key == k {
			return s.QUIC
		}
	}
	return false
}

func waitQUIC(t *testing.T, m *Mesh, k Key) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !quicTo(m, k) {
		if time.Now().After(deadline) {
			t.Fatal("15 秒内没有建好 QUIC 连接")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestQUIC：两端都支持、直连时，mesh 上的 TCP 连接走 QUIC；对方照常按 mesh 地址认出调用方，
// 半关闭（CloseWrite）以后还能收到回复。
func TestQUIC(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	ln, err := b.m.Listen(7100)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	caller := make(chan Key, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		k, _ := b.m.KeyOf(c.RemoteAddr())
		caller <- k
		got, _ := io.ReadAll(c) // 读到对方 CloseWrite
		_, _ = c.Write(append([]byte("got "), got...))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 第一次 Dial 可能赶在 QUIC 建好之前，先等它建好。
	_, _ = a.m.quic.open(ctx, b.m.PublicKey(), 7199)
	waitQUIC(t, a.m, b.m.PublicKey())
	c, err := a.m.Dial(ctx, b.m.PublicKey(), 7100)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*quicStream); !ok {
		t.Fatalf("应该走 QUIC，实际是 %T", c)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != "got hello" {
		t.Fatalf("半关闭以后的回复不对：%q %v", reply, err)
	}
	if k := <-caller; k != a.m.PublicKey() {
		t.Fatal("对方没有认出调用方")
	}
}

// TestQUICOldPeer：对端不支持 QUIC（旧版本）时走 WireGuard 里的 TCP。
func TestQUICOldPeer(t *testing.T) {
	dropUDP = nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var pa, pb Key
	_, _ = rand.Read(pa[:])
	_, _ = rand.Read(pb[:])
	ma, err := New(Config{PrivateKey: pa, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer ma.Close()
	mb, err := New(Config{PrivateKey: pb, NoQUIC: true, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	_ = ma.SetPeers([]Peer{{Key: mb.PublicKey(), Endpoints: local(mb.Port())}})
	_ = mb.SetPeers([]Peer{{Key: ma.PublicKey(), Endpoints: local(ma.Port())}})
	waitPath(t, ma, mb.PublicKey(), "direct")
	time.Sleep(3 * time.Second) // 等能力包
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := mb.Listen(7100)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = io.Copy(c, c)
		}
	}()
	c, err := ma.Dial(ctx, mb.PublicKey(), 7100)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*quicStream); ok || quicTo(ma, mb.PublicKey()) {
		t.Fatal("对端不支持 QUIC，不应该用")
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("回显不对：%q %v", buf, err)
	}
}

// TestQUICBadProof：拿不出和对方的共享密钥（冒充别人）时，QUIC 连接建不起来。
func TestQUICBadProof(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	addr, _, ok := a.m.quic.peerFor(b.m.PublicKey())
	for i := 0; !ok && i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		addr, _, ok = a.m.quic.peerFor(b.m.PublicKey())
	}
	if !ok {
		t.Fatal("B 应该支持 QUIC")
	}
	var wrong [32]byte
	_, _ = rand.Read(wrong[:])
	if _, err := a.m.quic.dial(context.Background(), b.m.PublicKey(), addr, wrong); err == nil {
		t.Fatal("身份证明不对也连上了")
	}
}

var _ net.Conn = (*quicStream)(nil)
