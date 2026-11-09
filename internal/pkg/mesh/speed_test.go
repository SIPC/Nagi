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
	if r.Upload <= 0 || r.Download <= 0 || !r.QUIC || r.QUICOffHere || r.QUICOffPeer {
		t.Fatalf("结果不对：%+v", r)
	}
	// 走 QUIC 时还经 WireGuard 测一次对比。
	if wg := r.WireGuard; wg == nil || wg.QUIC || wg.Upload <= 0 || wg.Download <= 0 {
		t.Fatalf("没有经 WireGuard 对比：%+v", r.WireGuard)
	}
	t.Logf("QUIC 上传 %.1f MB/s，下载 %.1f MB/s；WireGuard 上传 %.1f MB/s，下载 %.1f MB/s",
		r.Upload/1e6, r.Download/1e6, r.WireGuard.Upload/1e6, r.WireGuard.Download/1e6)
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
	if r.QUIC || r.Upload <= 0 || r.Download <= 0 || r.WireGuard != nil {
		t.Fatalf("应该走 WireGuard：%+v", r)
	}
	if !r.QUICOffPeer || r.QUICOffHere {
		t.Fatalf("应该说对方关了 QUIC：%+v", r)
	}
	if rb, err := SpeedTest(ctx, b.m, a.m.PublicKey(), 500*time.Millisecond); err == nil && !rb.QUICOffHere {
		t.Fatalf("B 应该说自己关了 QUIC：%+v", rb)
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

// 同一个成员最多连着测 speedBurst 轮，之后每 speedRefill 补一轮；别的成员不受影响。
func TestSpeedLimiter(t *testing.T) {
	l := newSpeedLimiter()
	now := time.Now()
	a, b := Key{1}, Key{2}
	for i := range speedBurst {
		if !l.take(a, now) {
			t.Fatalf("第 %d 轮应该放行", i+1)
		}
	}
	if l.take(a, now) {
		t.Fatal("用完以后应该拒绝")
	}
	if !l.take(b, now) {
		t.Fatal("别的成员不受影响")
	}
	if l.take(a, now.Add(speedRefill-time.Second)) {
		t.Fatal("还没补上一轮")
	}
	if !l.take(a, now.Add(speedRefill)) || l.take(a, now.Add(speedRefill)) {
		t.Fatal("过了 speedRefill 应该正好补一轮")
	}
}

// TestSpeedLimited：测得太频繁时对方拒绝，报 ErrSpeedLimited。
func TestSpeedLimited(t *testing.T) {
	dropUDP = nil
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	serveSpeedFor(t, b.m)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range speedBurst {
		if _, _, err := speedRound(ctx, a.m.Dial, b.m.PublicKey(), 'd', 100*time.Millisecond); err != nil {
			t.Fatalf("第 %d 轮：%v", i+1, err)
		}
	}
	if _, _, err := speedRound(ctx, a.m.Dial, b.m.PublicKey(), 'd', 100*time.Millisecond); !errors.Is(err, ErrSpeedLimited) {
		t.Fatalf("应该说测速太频繁，实际 %v", err)
	}
}
