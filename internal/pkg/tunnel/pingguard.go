package tunnel

import (
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// yamux v0.1.2 的帧头：版本（1 字节）、类型（1）、标志（2）、通道 ID（4）、长度（4），多字节字段是大端。
// 只有数据帧后面跟着 length 字节的内容；ping 帧的 length 是 ping 的编号。
const (
	frameHeaderSize = 12
	frameData       = 0
	framePing       = 2
	flagSYN         = 1
)

const (
	// pingRate、pingBurst 限制对方发来的 ping 请求：每秒最多 pingRate 个，允许 pingBurst 个的突发。
	// yamux 对每个 ping 请求都起一个协程回复，发送队列满时协程最多等 ConnectionWriteTimeout，
	// 不限的话几 MB 的 ping 就能堆出几十万个协程。正常情况下两端各自每 KeepAliveInterval 发一个。
	pingRate  = 10
	pingBurst = 20
)

// ErrTooManyPings 表示对方发来的 ping 超出频率，会话已经断开。
var ErrTooManyPings = errors.New("对方发来的 ping 过多，已断开")

// pingGuard 在交给 yamux 的连接上数对方发来的 ping 请求，超出频率时读取返回错误，yamux 随即关闭会话。
// 读到的字节原样交给 yamux，只在旁边跟踪帧的边界。只有 yamux 的接收协程读它，不用加锁。
type pingGuard struct {
	net.Conn

	hdr  [frameHeaderSize]byte
	hdrN int    // hdr 里已经收到的字节数
	skip uint64 // 当前数据帧还没读到的内容字节数

	tokens  float64
	last    time.Time
	nowFunc func() time.Time
	err     error
}

func newPingGuard(c net.Conn) *pingGuard {
	return &pingGuard{Conn: c, tokens: pingBurst, nowFunc: time.Now}
}

func (g *pingGuard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.Conn.Read(p)
	if !g.scan(p[:n]) {
		g.err = ErrTooManyPings
		return 0, g.err
	}
	return n, err
}

// scan 跟踪 b 里的帧边界，返回 ping 请求是否还在频率以内。帧头可能跨两次读取。
func (g *pingGuard) scan(b []byte) bool {
	for len(b) > 0 {
		if g.skip > 0 {
			k := uint64(len(b))
			if k > g.skip {
				k = g.skip
			}
			g.skip -= k
			b = b[k:]
			continue
		}
		k := copy(g.hdr[g.hdrN:], b)
		g.hdrN += k
		b = b[k:]
		if g.hdrN < frameHeaderSize {
			break
		}
		g.hdrN = 0
		typ := g.hdr[1]
		flags := binary.BigEndian.Uint16(g.hdr[2:4])
		length := binary.BigEndian.Uint32(g.hdr[8:12])
		switch {
		case typ == frameData:
			g.skip = uint64(length)
		case typ == framePing && flags&flagSYN != 0:
			if !g.allowPing() {
				return false
			}
		}
	}
	return true
}

// allowPing 是令牌桶：每个 ping 请求用掉一个令牌，令牌按 pingRate 每秒补充，最多 pingBurst 个。
func (g *pingGuard) allowPing() bool {
	now := g.nowFunc()
	if !g.last.IsZero() {
		g.tokens += now.Sub(g.last).Seconds() * pingRate
		if g.tokens > pingBurst {
			g.tokens = pingBurst
		}
	}
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}
