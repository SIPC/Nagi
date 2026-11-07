package mesh

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/netip"
	"testing"
	"time"
)

// group 发一组 n 个随机内容的包，返回原包和发出去的 FEC 包（数据包、校验包）。
func group(enc *fecEncoder, n int) (pkts, frames [][]byte) {
	for i := 0; i < n; i++ {
		p := make([]byte, 100+i*37)
		_, _ = rand.Read(p)
		pkts = append(pkts, p)
		data, parity := enc.frame(p, time.Now(), func() {})
		frames = append(frames, data)
		frames = append(frames, parity...)
	}
	return pkts, frames
}

// TestFECRecover：一组 10 个包加 2 个校验包，丢掉任意 2 个数据包都能算回来，并且按原来的顺序交出。
func TestFECRecover(t *testing.T) {
	pkts, frames := group(&fecEncoder{parity: 2}, fecGroup)
	if len(frames) != fecGroup+2 {
		t.Fatalf("应该有 %d 个包，实际 %d 个", fecGroup+2, len(frames))
	}
	dec := newFECDecoder()
	var got [][]byte
	for i, f := range frames {
		if i == 3 || i == 7 { // 丢掉两个数据包
			continue
		}
		out, waiting := dec.push(f, time.Now())
		got = append(got, out...)
		if i > 3 && i < fecGroup && len(out) != 0 {
			t.Fatalf("第 3 个包还没恢复，第 %d 个包不应该先交出去", i)
		}
		if i == len(frames)-1 && waiting {
			t.Fatal("全部恢复以后不应该还扣着包")
		}
	}
	if len(got) != fecGroup {
		t.Fatalf("应该交出 %d 个包，实际 %d 个", fecGroup, len(got))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i]) {
			t.Fatalf("第 %d 个包不对（顺序或内容）", i)
		}
	}
}

// TestFECPeerRestart：对方重启后编码器换了一个组号接着发（离原来的很远），接收方要从新的组号重新开始：
// 新组照常按顺序交、丢的包照常算回来，不会因为组号比记着的旧，建好就当过期的删掉。
func TestFECPeerRestart(t *testing.T) {
	dec := newFECDecoder()
	send := func(enc *fecEncoder, groups int) {
		t.Helper()
		for g := 0; g < groups; g++ {
			pkts, frames := group(enc, fecGroup)
			var got [][]byte
			for i, f := range frames {
				if i == 4 { // 每组丢一个数据包
					continue
				}
				out, _ := dec.push(f, time.Now())
				got = append(got, out...)
			}
			if len(got) != fecGroup {
				t.Fatalf("组号 %d 起的第 %d 组：应该交出 %d 个包，实际 %d 个", enc.gid, g, fecGroup, len(got))
			}
			for i := range pkts {
				if !bytes.Equal(got[i], pkts[i]) {
					t.Fatalf("组号 %d 起的第 %d 组：第 %d 个包不对（顺序或内容）", enc.gid, g, i)
				}
			}
		}
	}
	send(&fecEncoder{parity: 1, gid: 100000}, 5)
	// 重启：组号比记着的小得多（以前从 0 开始）、或者大得多（现在从随机值开始）。
	send(&fecEncoder{parity: 1, gid: 7}, 5)
	send(&fecEncoder{parity: 1, gid: 1 << 30}, 5)
}

// TestFECOverhead：两端记下的 FEC 多出的字节数加上交给 WireGuard 的字节数，等于线上实际收发的字节数。
func TestFECOverhead(t *testing.T) {
	enc := &fecEncoder{parity: 2}
	pkts, frames := group(enc, fecGroup)
	sent, plain := 0, 0
	for _, f := range frames {
		sent += len(f)
	}
	for _, p := range pkts {
		plain += len(p)
	}
	if got := enc.overheadBytes(); got != uint64(sent-plain) {
		t.Fatalf("发送方多出的字节数应该是 %d，实际 %d", sent-plain, got)
	}
	dec := newFECDecoder()
	wire, delivered := 0, 0
	for i, f := range frames {
		if i == 3 { // 丢一个数据包，由校验包恢复：WireGuard 收到了，线上没有
			continue
		}
		wire += len(f)
		out, _ := dec.push(f, time.Now())
		for _, x := range out {
			delivered += len(x)
		}
	}
	if delivered != plain {
		t.Fatalf("应该交出 %d 字节，实际 %d", plain, delivered)
	}
	if got := dec.overheadBytes(); got != int64(wire-delivered) {
		t.Fatalf("接收方多出的字节数应该是 %d，实际 %d", wire-delivered, got)
	}
}

// TestFECRelease：丢得太多、算不回来时，扣着的包等 fecHold 后按顺序放行。
func TestFECRelease(t *testing.T) {
	pkts, frames := group(&fecEncoder{parity: 1}, fecGroup)
	dec := newFECDecoder()
	now := time.Now()
	var got [][]byte
	for i, f := range frames {
		if i == 2 || i == 5 { // 丢两个，只有一个校验包，算不回来
			continue
		}
		out, _ := dec.push(f, now)
		got = append(got, out...)
	}
	if len(got) != 2 {
		t.Fatalf("缺包前的 2 个包应该先交出去，实际交了 %d 个", len(got))
	}
	if out, _ := dec.release(now.Add(fecHold / 2)); len(out) != 0 {
		t.Fatal("还没到 fecHold 就放行了")
	}
	out, waiting := dec.release(now.Add(fecHold))
	got = append(got, out...)
	if !waiting {
		t.Fatal("后面还缺第 5 个包，应该接着扣着")
	}
	out, waiting = dec.release(now.Add(2 * fecHold))
	got = append(got, out...)
	if waiting || len(got) != fecGroup-2 {
		t.Fatalf("放行以后应该交出 %d 个包，实际 %d 个", fecGroup-2, len(got))
	}
	want := append(append(append([][]byte{}, pkts[:2]...), pkts[3:5]...), pkts[6:]...)
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("第 %d 个交出的包不对", i)
		}
	}
}

// TestFECFlush：组没凑满时，等 fecFlush 以后按已有的包发校验，丢掉的包也能算回来。
func TestFECFlush(t *testing.T) {
	enc := &fecEncoder{parity: 1}
	dec := newFECDecoder()
	_, _ = enc.frame([]byte("first"), time.Now(), func() {})
	b, _ := enc.frame([]byte("second"), time.Now(), func() {})
	if enc.flush(time.Now()) != nil {
		t.Fatal("还没到 fecFlush 就发了校验包")
	}
	parity := enc.flush(time.Now().Add(fecFlush))
	if len(parity) != 1 {
		t.Fatalf("应该有 1 个校验包，实际 %d 个", len(parity))
	}
	if out, _ := dec.push(b, time.Now()); len(out) != 0 {
		t.Fatal("第一个包丢了，第二个包应该先扣着")
	}
	out, waiting := dec.push(parity[0], time.Now())
	if len(out) != 2 || string(out[0]) != "first" || string(out[1]) != "second" || waiting {
		t.Fatalf("应该按顺序交出两个包：%q", out)
	}
}

// TestFECAdjust：丢包变多立刻加校验包，变少要连续几次报告才减。
func TestFECAdjust(t *testing.T) {
	enc := &fecEncoder{parity: 1}
	enc.adjust(30000, time.Now())
	if enc.parityCount() != 3 {
		t.Fatal("丢包变多应该立刻加校验包")
	}
	for i := 1; i < fecLowerAfter; i++ {
		enc.adjust(0, time.Now())
		if enc.parityCount() != 3 {
			t.Fatalf("第 %d 次报告丢包少就减了", i)
		}
	}
	enc.adjust(0, time.Now())
	if enc.parityCount() != 0 {
		t.Fatal("连续几次丢包少以后应该减到 0")
	}
}

// TestFECUseless：成串地丢（一组丢 3 个）时报告校验包没用，发送方暂停 fecPause。
func TestFECUseless(t *testing.T) {
	enc := &fecEncoder{parity: 1}
	dec := newFECDecoder()
	for g := 0; g < 300; g++ {
		for i := 0; i < fecGroup; i++ {
			data, parity := enc.frame([]byte("x"), time.Now(), func() {})
			if g%5 != 0 || i > 2 {
				dec.push(data, time.Now())
			}
			for _, x := range parity {
				dec.push(x, time.Now())
			}
		}
	}
	if _, ok, useless := dec.lossPPM(); !ok || !useless {
		t.Fatal("成串丢包时应该报告校验包没用")
	}
	// 随机丢包（一组最多丢 1 个）不算。
	dec = newFECDecoder()
	for g := 0; g < 300; g++ {
		for i := 0; i < fecGroup; i++ {
			data, _ := enc.frame([]byte("x"), time.Now(), func() {})
			if g%3 != 0 || i != 4 {
				dec.push(data, time.Now())
			}
		}
	}
	if _, ok, useless := dec.lossPPM(); !ok || useless {
		t.Fatal("零散的丢包不应该算成校验包没用")
	}
	now := time.Now()
	enc.giveUp(now)
	enc.adjust(30000, now.Add(fecPause/2))
	if enc.parityCount() != 0 {
		t.Fatal("暂停期间不应该加校验包")
	}
	enc.adjust(30000, now.Add(fecPause))
	if enc.parityCount() != 3 {
		t.Fatal("暂停结束后应该按丢包率加回来")
	}
}

// TestFECNoFlushWithoutParity：没有校验包的组不提前结束，否则接收方会把没发的包当成丢了；
// 一组的校验包个数在开始时定下，中途调整只影响下一组。
func TestFECNoFlushWithoutParity(t *testing.T) {
	enc := &fecEncoder{}
	_, _ = enc.frame([]byte("a"), time.Now(), func() {})
	enc.setParity(1)
	if enc.flush(time.Now().Add(fecFlush)) != nil || enc.n != 1 {
		t.Fatal("没有校验包的组不应该提前结束")
	}
	for i := 1; i < fecGroup; i++ {
		_, _ = enc.frame([]byte("a"), time.Now(), func() {})
	}
	data, _ := enc.frame([]byte("b"), time.Now(), func() {})
	if data[6]&fecProtect == 0 {
		t.Fatal("调整以后的下一组应该有校验包")
	}
}

// TestFECLoss：接收方按组统计丢包率，parityFor 按丢包率给校验包个数。
func TestFECLoss(t *testing.T) {
	enc := &fecEncoder{}
	dec := newFECDecoder()
	sent := 0
	for g := 0; g < 300; g++ {
		for i := 0; i < fecGroup; i++ {
			data, _ := enc.frame([]byte("x"), time.Now(), func() {})
			sent++
			if sent%50 != 0 { // 丢 2%
				if out, _ := dec.push(data, time.Now()); len(out) != 1 {
					t.Fatal("没有校验包的组应该收到就交")
				}
			}
		}
	}
	ppm, ok, _ := dec.lossPPM()
	if !ok || ppm < 15000 || ppm > 25000 {
		t.Fatalf("丢包率应该在 2%% 左右，实际 %d ppm（ok=%v）", ppm, ok)
	}
	if parityFor(ppm) != 2 || parityFor(100) != 0 || parityFor(2000) != 1 || parityFor(30000) != 3 {
		t.Fatal("parityFor 不对")
	}
}

// TestFECLossyLink：直连随机丢 2% 的包，FEC 自动开启并恢复丢包，数据原样到达。
func TestFECLossyLink(t *testing.T) {
	testNoQUIC = true // QUIC 不经过 WireGuard，也就没有 FEC
	defer func() { dropUDP, testNoQUIC = nil, false }()
	_, a, b := setupWith(t, func(_, a, b testNode) {
		dropUDP = func(from uint16, to netip.AddrPort) bool {
			between := (from == a.port && to.Port() == b.port) || (from == b.port && to.Port() == a.port)
			return between && mrand.Float64() < 0.02
		}
	})
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	ln, err := b.m.Listen(7100)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	want := make([]byte, 8<<20)
	_, _ = rand.Read(want)
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		got <- data
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := a.m.Dial(ctx, b.m.PublicKey(), 7100)
	if err != nil {
		t.Fatal(err)
	}
	// 分两段发：第一段让 B 统计到丢包率、报给 A，第二段时 A 已经开了 FEC。
	if _, err := c.Write(want[:4<<20]); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for fecTo(a.m, b.m.PublicKey()) <= 0 && time.Now().Before(deadline) {
		_, _ = c.Write(want[4<<20 : 4<<20+64<<10])
		want = append(want[:4<<20+64<<10:4<<20+64<<10], want[4<<20:]...)
		time.Sleep(200 * time.Millisecond)
	}
	if fecTo(a.m, b.m.PublicKey()) <= 0 {
		t.Fatal("丢包 2% 时没有开启 FEC")
	}
	if _, err := c.Write(want[len(want)-4<<20:]); err != nil {
		t.Fatal(err)
	}
	c.Close()
	data := <-got
	if !bytes.Equal(data, want) {
		t.Fatalf("收到的数据不对：%d / %d 字节", len(data), len(want))
	}
	if recoveredFrom(b.m, a.m.PublicKey()) == 0 {
		t.Fatal("开了 FEC 但一个包都没恢复")
	}
	// 两个方向的丢包率：A 发给 B 的（B 报告的）、B 收 A 的（B 自己算的），都应该在 2% 左右。
	// 丢包率随能力包每 2 秒算一次，传得快时数据已经收完还没算过，等它出来。
	tx, rx := int64(-1), int64(-1)
	for deadline := time.Now().Add(10 * time.Second); (tx < 0 || rx < 0) && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		for _, s := range a.m.Status() {
			if s.Key == b.m.PublicKey() {
				tx = s.TxLoss
			}
		}
		for _, s := range b.m.Status() {
			if s.Key == a.m.PublicKey() {
				rx = s.RxLoss
			}
		}
	}
	for name, v := range map[string]int64{"A 发出": tx, "B 收到": rx} {
		if v < 5_000 || v > 50_000 {
			t.Errorf("%s的丢包率应该在 2%% 左右，实际 %d ppm", name, v)
		}
	}
}

// TestFECOldPeer：对端不支持 FEC（旧版本）时不加 FEC 包头，照常能通。
func TestFECOldPeer(t *testing.T) {
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
	mb, err := New(Config{PrivateKey: pb, NoFEC: true, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	_ = ma.SetPeers([]Peer{{Key: mb.PublicKey(), Endpoints: local(mb.Port())}})
	_ = mb.SetPeers([]Peer{{Key: ma.PublicKey(), Endpoints: local(ma.Port())}})
	a, b := testNode{m: ma, port: ma.Port()}, testNode{m: mb, port: mb.Port()}
	waitPath(t, ma, mb.PublicKey(), "direct")
	echo(t, a, b)
	if fecTo(ma, mb.PublicKey()) != -1 {
		t.Fatal("对端不支持 FEC，不应该用")
	}
}

func fecTo(m *Mesh, k Key) int {
	for _, s := range m.Status() {
		if s.Key == k {
			return s.FEC
		}
	}
	return -2
}

func recoveredFrom(m *Mesh, k Key) uint64 {
	for _, s := range m.Status() {
		if s.Key == k {
			return s.Recovered
		}
	}
	return 0
}
