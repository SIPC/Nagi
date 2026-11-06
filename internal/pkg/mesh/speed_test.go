package mesh

import (
	"context"
	"errors"
	mrand "math/rand/v2"
	"net/netip"
	"testing"
	"time"
)

func serveSpeedFor(t *testing.T, m *Mesh) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = serveSpeed(ctx, m) }()
	time.Sleep(50 * time.Millisecond)
}

// TestSpeed：两个方向都测得出速率，直连且两端支持时走 QUIC。
func TestSpeed(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	serveSpeedFor(t, b.m)
	_, _ = a.m.quic.open(context.Background(), b.m.PublicKey(), 7199)
	waitQUIC(t, a.m, b.m.PublicKey())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := SpeedTest(ctx, a.m, b.m.PublicKey(), 2500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if r.Upload <= 0 || r.Download <= 0 || !r.QUIC {
		t.Fatalf("结果不对：%+v", r)
	}
	t.Logf("上传 %.1f MB/s，下载 %.1f MB/s", r.Upload/1e6, r.Download/1e6)
	// 经 QUIC 的流量也算进收发字节（概览的流量图靠它）。
	for _, s := range a.m.Status() {
		if s.Key == b.m.PublicKey() && (s.TxBytes < 1<<20 || s.RxBytes < 1<<20) {
			t.Fatalf("QUIC 的流量没有算进去：收 %d，发 %d", s.RxBytes, s.TxBytes)
		}
	}
}

// TestSpeedNoQUIC：关掉 QUIC 以后测速走 WireGuard；再打开又走 QUIC。
func TestSpeedNoQUIC(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	serveSpeedFor(t, b.m)
	_, _ = a.m.quic.open(context.Background(), b.m.PublicKey(), 7199)
	waitQUIC(t, a.m, b.m.PublicKey())

	b.m.SetQUIC(false)
	if b.m.QUIC() || quicTo(b.m, a.m.PublicKey()) {
		t.Fatal("关掉以后不应该还有 QUIC 连接")
	}
	time.Sleep(3 * time.Second) // 等能力包告诉 A
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := SpeedTest(ctx, a.m, b.m.PublicKey(), 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if r.QUIC || r.Upload <= 0 || r.Download <= 0 {
		t.Fatalf("应该走 WireGuard：%+v", r)
	}

	b.m.SetQUIC(true)
	waitQUIC(t, a.m, b.m.PublicKey())
	r, err = SpeedTest(ctx, a.m, b.m.PublicKey(), 1500*time.Millisecond)
	if err != nil || !r.QUIC {
		t.Fatalf("重新打开以后应该走 QUIC：%+v %v", r, err)
	}
}

// TestSpeedBusy：对方正在测时拒绝第二个。
func TestSpeedBusy(t *testing.T) {
	dropUDP = nil
	p, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	waitPath(t, p.m, b.m.PublicKey(), "direct")
	serveSpeedFor(t, b.m)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SpeedTest(ctx, a.m, b.m.PublicKey(), 3*time.Second)
		done <- err
	}()
	time.Sleep(time.Second)
	if _, err := SpeedTest(ctx, p.m, b.m.PublicKey(), time.Second); !errors.Is(err, ErrSpeedBusy) {
		t.Fatalf("应该说对方正在测速，实际 %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestSpeedOldPeer：对方没有测速服务（旧版本）时，两种路都报不支持。
func TestSpeedOldPeer(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := SpeedTest(ctx, a.m, b.m.PublicKey(), time.Second); !errors.Is(err, ErrSpeedUnsupported) {
		t.Fatalf("QUIC：应该说不支持，实际 %v", err)
	}
	a.m.SetQUIC(false)
	if _, err := SpeedTest(ctx, a.m, b.m.PublicKey(), time.Second); !errors.Is(err, ErrSpeedUnsupported) {
		t.Fatalf("WireGuard：应该说不支持，实际 %v", err)
	}
}

// TestQUICLoss：走 QUIC 时，发给对方的丢包率来自 QUIC 自己的统计。
func TestQUICLoss(t *testing.T) {
	defer func() { dropUDP = nil }()
	_, a, b := setupWith(t, func(_, a, b testNode) {
		dropUDP = func(from uint16, to netip.AddrPort) bool {
			return from == a.port && to.Port() == b.port && mrand.Float64() < 0.02
		}
	})
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	serveSpeedFor(t, b.m)
	_, _ = a.m.quic.open(context.Background(), b.m.PublicKey(), 7199)
	waitQUIC(t, a.m, b.m.PublicKey())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := SpeedTest(ctx, a.m, b.m.PublicKey(), 1500*time.Millisecond)
	if err != nil || !r.QUIC {
		t.Fatalf("%+v %v", r, err)
	}
	for _, s := range a.m.Status() {
		if s.Key == b.m.PublicKey() && (s.TxLoss < 5_000 || s.TxLoss > 50_000) {
			t.Fatalf("丢包率应该在 2%% 左右，实际 %d ppm", s.TxLoss)
		}
	}
}
