package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

// 测速（见 docs/mesh.md#测速）：经 mesh 和某个成员互传数据，量两个方向的吞吐。走的是和业务一样的路
// （能走 QUIC 就走 QUIC），测出来的就是容器跨节点访问、迁移、同步时能拿到的速度。
//
// 协议：调用方连对方的 SpeedPort，发一个字节的方向和四个字节的时长（毫秒）。对方先回一个字节：0 接受，1 正在测（拒绝），
// 2 这个成员测得太频繁（拒绝）。
//   - 上传（'u'）：调用方发 duration，然后半关闭；对方读到结束，回 8 字节的速率（字节/秒，float64 的位）。
//   - 下载（'d'）：对方发 duration，然后半关闭；调用方自己算。
//
// 速率在收的一端算，并跳过最开始的 speedWarmup（慢启动），只算之后的。
const (
	// SpeedPort 是每个成员在 mesh 地址上提供测速的 TCP 端口。
	SpeedPort = 7002
	// speedMax：一次测试单个方向最长这么久，调用方要求再长也按这个算。
	speedMax    = 10 * time.Second
	speedWarmup = time.Second
	speedChunk  = 64 << 10
	// speedBurst、speedRefill：每个成员最多连着测 speedBurst 轮（一次完整的测速：平时那条路和 WireGuard
	// 各上传、下载一轮），之后每 speedRefill 补一轮。免得某个成员一直占着测速、刷流量。
	speedBurst  = 4
	speedRefill = 30 * time.Second
)

// ErrSpeedLimited：本节点向对方测速太频繁。
var ErrSpeedLimited = errors.New("测速太频繁")

// ErrSpeedBusy：对方正在和别的成员测速。
var ErrSpeedBusy = errors.New("对方正在测速")

// ErrSpeedUnsupported：对方不支持测速（版本较旧）。
var ErrSpeedUnsupported = errors.New("对方不支持测速")

// SpeedResult 是一次测速的结果，速率单位是字节/秒。
type SpeedResult struct {
	Upload, Download float64
	// QUIC：这次测速走的是 QUIC。
	QUIC bool
	// WireGuard：当前走 QUIC 时，再经 WireGuard 测一次的结果，用来对比；没测成时为 nil。
	WireGuard *SpeedResult
	// QUICOffHere、QUICOffPeer：直连时这一端、对方关闭了 QUIC 快速通道（对方是旧版本时不算）。
	QUICOffHere, QUICOffPeer bool
}

type dialer func(ctx context.Context, k Key, port uint16) (net.Conn, error)

// serveSpeed 在 SpeedPort 上接受测速，同一时间只测一个，直到 ctx 取消。
func serveSpeed(ctx context.Context, m *Mesh) error {
	ln, err := m.Listen(SpeedPort)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var busy sync.Mutex
	limit := newSpeedLimiter()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			var hdr [5]byte
			if _, err := io.ReadFull(c, hdr[:]); err != nil {
				return
			}
			caller, ok := m.KeyOf(c.RemoteAddr())
			if !ok {
				return
			}
			if !busy.TryLock() {
				_, _ = c.Write([]byte{1})
				return
			}
			defer busy.Unlock()
			if !limit.take(caller, time.Now()) {
				_, _ = c.Write([]byte{2})
				return
			}
			d := min(time.Duration(binary.BigEndian.Uint32(hdr[1:]))*time.Millisecond, speedMax)
			if _, err := c.Write([]byte{0}); err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(d + 30*time.Second))
			switch hdr[0] {
			case 'u':
				rate, err := receive(c)
				if err != nil {
					return
				}
				var out [8]byte
				binary.BigEndian.PutUint64(out[:], math.Float64bits(rate))
				_, _ = c.Write(out[:])
			case 'd':
				_ = send(c, d)
			}
		}()
	}
}

// SpeedTest 和 k 互传数据，先上传再下载，每个方向 d（最长 speedMax）。走的是平时的路（Dial）；
// 走的是 QUIC 时再经 WireGuard 测一次对比，对比没测成不算失败。
func SpeedTest(ctx context.Context, m *Mesh, k Key, d time.Duration) (SpeedResult, error) {
	r, err := speedPair(ctx, m, k, d, m.Dial)
	if err != nil {
		return r, err
	}
	if r.QUIC {
		if wg, err := speedPair(ctx, m, k, d, m.dialWireGuard); err == nil && !wg.QUIC {
			r.WireGuard = &wg
		}
	}
	r.QUICOffHere, r.QUICOffPeer = m.quicOff(k)
	return r, nil
}

// SpeedTestVia 只测一种走法：wireguard 为 false 时走平时的路，为 true 时经 WireGuard 隧道（用来和 QUIC 对比）。
// 界面分步测时用，每一步测完就能显示。
func SpeedTestVia(ctx context.Context, m *Mesh, k Key, d time.Duration, wireguard bool) (SpeedResult, error) {
	dial := m.Dial
	if wireguard {
		dial = m.dialWireGuard
	}
	r, err := speedPair(ctx, m, k, d, dial)
	if err != nil {
		return r, err
	}
	r.QUICOffHere, r.QUICOffPeer = m.quicOff(k)
	return r, nil
}

func speedPair(ctx context.Context, m *Mesh, k Key, d time.Duration, dial dialer) (SpeedResult, error) {
	d = min(d, speedMax)
	var r SpeedResult
	up, viaQUIC, err := speedRound(ctx, dial, k, 'u', d)
	if err != nil {
		return r, err
	}
	down, _, err := speedRound(ctx, dial, k, 'd', d)
	if err != nil {
		return r, err
	}
	return SpeedResult{Upload: up, Download: down, QUIC: viaQUIC}, nil
}

func speedRound(ctx context.Context, dial dialer, k Key, dir byte, d time.Duration) (float64, bool, error) {
	c, err := dial(ctx, k, SpeedPort)
	if err != nil {
		if strings.Contains(err.Error(), "refused") {
			return 0, false, ErrSpeedUnsupported
		}
		return 0, false, err
	}
	defer c.Close()
	_, viaQUIC := c.(*quicStream)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(d + 30*time.Second))
	var hdr [5]byte
	hdr[0] = dir
	binary.BigEndian.PutUint32(hdr[1:], uint32(d.Milliseconds()))
	if _, err := c.Write(hdr[:]); err != nil {
		return 0, viaQUIC, err
	}
	var ack [1]byte
	if _, err := io.ReadFull(c, ack[:]); err != nil {
		// 旧版本没有这个端口：gVisor TCP 连不上，QUIC 的流被对方重置。
		return 0, viaQUIC, ErrSpeedUnsupported
	}
	switch ack[0] {
	case 0:
	case 2:
		return 0, viaQUIC, ErrSpeedLimited
	default:
		return 0, viaQUIC, ErrSpeedBusy
	}
	if dir == 'd' {
		rate, err := receive(c)
		return rate, viaQUIC, err
	}
	if err := send(c, d); err != nil {
		return 0, viaQUIC, err
	}
	var out [8]byte
	if _, err := io.ReadFull(c, out[:]); err != nil {
		return 0, viaQUIC, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(out[:])), viaQUIC, nil
}

// send 发 d 这么久，然后半关闭。
func send(c net.Conn, d time.Duration) error {
	buf := make([]byte, speedChunk)
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if _, err := c.Write(buf); err != nil {
			return err
		}
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// receive 读到对方半关闭，返回跳过开头 speedWarmup 以后的速率。整个过程不到两倍 speedWarmup 时按全程算。
func receive(c net.Conn) (float64, error) {
	buf := make([]byte, speedChunk)
	var total, atWarm int64
	var start, warm time.Time
	for {
		n, err := c.Read(buf)
		now := time.Now()
		if n > 0 {
			if start.IsZero() {
				start = now
			}
			total += int64(n)
			if warm.IsZero() && now.Sub(start) >= speedWarmup {
				warm, atWarm = now, total
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	end := time.Now()
	if start.IsZero() {
		return 0, nil
	}
	if !warm.IsZero() && end.Sub(warm) >= speedWarmup {
		return float64(total-atWarm) / end.Sub(warm).Seconds(), nil
	}
	return float64(total) / max(end.Sub(start).Seconds(), 0.001), nil
}

// speedLimiter 按成员限制测速的次数（令牌桶，见 speedBurst）。
type speedLimiter struct {
	mu      sync.Mutex
	buckets map[Key]speedBucket
}

type speedBucket struct {
	tokens float64
	at     time.Time
}

func newSpeedLimiter() *speedLimiter { return &speedLimiter{buckets: map[Key]speedBucket{}} }

// take 给 k 用掉一轮，用完了返回 false。
func (l *speedLimiter) take(k Key, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	fill := func(b speedBucket) float64 {
		return min(speedBurst, b.tokens+float64(now.Sub(b.at))/float64(speedRefill))
	}
	// 补满的不用记着：成员会变，不清理的话一直留着。
	for key, b := range l.buckets {
		if key != k && fill(b) >= speedBurst {
			delete(l.buckets, key)
		}
	}
	b, ok := l.buckets[k]
	t := float64(speedBurst)
	if ok {
		t = fill(b)
	}
	if t < 1 {
		return false
	}
	l.buckets[k] = speedBucket{tokens: t - 1, at: now}
	return true
}
