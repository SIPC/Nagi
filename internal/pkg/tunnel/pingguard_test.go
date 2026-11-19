package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func frame(typ uint8, flags uint16, stream, length uint32) []byte {
	h := make([]byte, frameHeaderSize)
	h[1] = typ
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], stream)
	binary.BigEndian.PutUint32(h[8:12], length)
	return h
}

func pingSYN(id uint32) []byte { return frame(framePing, flagSYN, 0, id) }

// 对方不读回复、连续发 ping：面板断开会话，协程数不会涨上去。
func TestPingFloodClosesSession(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	sess, err := Server(a)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	before := runtime.NumGoroutine()

	sent := make(chan int, 1)
	go func() {
		n := 0
		for ; n < 100000; n++ {
			if _, err := b.Write(pingSYN(uint32(n))); err != nil {
				break
			}
		}
		sent <- n
	}()
	select {
	case <-sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("ping 洪泛没有断开会话")
	}
	if n := <-sent; n > 1000 {
		t.Fatalf("断开前收了 %d 个 ping", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+10 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: before=%d now=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 频率以内的 ping 和大量业务数据不受影响：数据帧的内容里像 ping 帧头的字节不算 ping。
func TestPingGuardKeepsNormalTraffic(t *testing.T) {
	panel, agent, ps := pair(t)
	for i := range pingBurst {
		if _, err := agent.Ping(); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
		if _, err := panel.Ping(); err != nil {
			t.Fatalf("reverse ping %d: %v", i, err)
		}
	}
	// 一条 1 MB 的上报，内容全是 ping 帧头。
	fake := strings.Repeat(string(pingSYN(7)), (1<<20)/frameHeaderSize)
	cc, err := Dial(agent)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 3 {
		if _, err := nagiv1.NewPanelServiceClient(cc).Report(ctx, &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{Hostname: fake}}); err != nil {
			t.Fatal(err)
		}
		if h := <-ps.got; h != fake {
			t.Fatal("hostname mismatch")
		}
	}
	if panel.IsClosed() || agent.IsClosed() {
		t.Fatal("会话被断开")
	}
}

// 帧头和数据帧的内容可能拆在任意两次读取之间；keepalive 频率的 ping 一直放行。
func TestPingGuardSplitFrames(t *testing.T) {
	var stream bytes.Buffer
	payload := bytes.Repeat(pingSYN(1), 100) // 数据帧的内容，不算 ping
	stream.Write(frame(frameData, flagSYN, 1, uint32(len(payload))))
	stream.Write(payload)
	stream.Write(frame(1, 0, 1, 1<<20)) // window update 没有内容，length 是窗口增量
	for i := range pingBurst {
		stream.Write(pingSYN(uint32(i)))
	}
	stream.Write(frame(framePing, 2, 0, 9)) // ping 回复不算
	stream.Write(frame(frameData, 0, 1, 0)) // 空的数据帧
	all := stream.Bytes()

	for _, chunk := range []int{1, 5, 11, 13, 1000} {
		g := newPingGuard(nil)
		now := time.Unix(1000, 0)
		g.nowFunc = func() time.Time { return now }
		for b := all; len(b) > 0; {
			k := min(chunk, len(b))
			if !g.scan(b[:k]) {
				t.Fatalf("chunk %d: 频率以内的 ping 被拒", chunk)
			}
			b = b[k:]
		}
		if g.hdrN != 0 || g.skip != 0 {
			t.Fatalf("chunk %d: 停在帧中间 hdrN=%d skip=%d", chunk, g.hdrN, g.skip)
		}
		// 令牌用完，同一时刻再来一个 ping 超出。
		if g.scan(pingSYN(99)) {
			t.Fatalf("chunk %d: 超出突发的 ping 被放行", chunk)
		}
	}

	// 两端各自每 15 秒发一个 keepalive ping，连续一天都放行。
	g := newPingGuard(nil)
	now := time.Unix(1000, 0)
	g.nowFunc = func() time.Time { return now }
	for i := range 24 * 3600 / 15 {
		now = now.Add(15 * time.Second)
		if !g.scan(pingSYN(uint32(i))) {
			t.Fatalf("keepalive %d 被拒", i)
		}
	}
	// 每秒 pingRate 个可以一直持续。
	for i := range 1000 {
		now = now.Add(time.Second / pingRate)
		if !g.scan(pingSYN(uint32(i))) {
			t.Fatalf("rate ping %d 被拒", i)
		}
	}
}

// 超出以后读取一直返回错误，读到的字节不再交给 yamux。
func TestPingGuardReadError(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	g := newPingGuard(a)
	go func() {
		var buf bytes.Buffer
		for i := range pingBurst + 1 {
			buf.Write(pingSYN(uint32(i)))
		}
		b.Write(buf.Bytes())
	}()
	p := make([]byte, 4096)
	total := 0
	for {
		n, err := g.Read(p)
		total += n
		if err != nil {
			if err != ErrTooManyPings {
				t.Fatalf("err = %v", err)
			}
			break
		}
	}
	if total > pingBurst*frameHeaderSize {
		t.Fatalf("超出后交出了 %d 字节", total)
	}
	if _, err := g.Read(p); err != ErrTooManyPings {
		t.Fatalf("second read: %v", err)
	}
}
