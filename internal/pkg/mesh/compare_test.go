package mesh

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestComparePaths(t *testing.T) {
	b := newBind(Key{8}, false, 0, 0, slog.Default())
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b.conns = []*net.UDPConn{c}
	port := uint16(c.LocalAddr().(*net.UDPAddr).Port)
	peer, pooled, outside := PublicKey(Key{16}), PublicKey(Key{24}), PublicKey(Key{32})
	on := true
	b.setPeers([]Peer{{Key: peer}, {Key: pooled, NodeRelayEnabled: &on, RelayPool: true}, {Key: outside, NodeRelayEnabled: &on}})
	addr := map[Key]netip.AddrPort{}
	for i, k := range []Key{peer, pooled, outside} {
		p := b.peers[k]
		p.direct, p.directAt, p.directEpoch = netip.MustParseAddrPort("127.0.0.1:"+string(rune('1'+i))+"000"), time.Now(), 3
		addr[k] = p.direct
	}
	m := &Mesh{bind: b}
	start := time.Now().Add(-time.Minute)

	// path 描述一条路线 60 轮里每一轮的结果：rtt 返回第 i 轮的延迟，0 表示超时。
	type path struct {
		via        Key
		small      func(i int) time.Duration
		large      func(i int) time.Duration
		err        error // 非空时第 i%3==0 轮换成这个本地错误（不是链路结果）
		epoch      uint64
		rounds     int
		roundEvery time.Duration
	}
	ms := func(n int) func(int) time.Duration {
		return func(int) time.Duration { return time.Duration(n) * time.Millisecond }
	}
	lossEvery := func(n, every int) func(int) time.Duration {
		return func(i int) time.Duration {
			if i%every == 0 {
				return 0
			}
			return time.Duration(n) * time.Millisecond
		}
	}
	build := func(paths ...path) []PathSample {
		var out []PathSample
		for _, p := range paths {
			hop := p.via
			if hop == (Key{}) {
				hop = peer
			}
			epoch, rounds, every := uint64(3), 60, time.Second
			if p.epoch != 0 {
				epoch = p.epoch
			}
			if p.rounds != 0 {
				rounds = p.rounds
			}
			if p.roundEvery != 0 {
				every = p.roundEvery
			}
			large := p.large
			if large == nil {
				large = p.small
			}
			for i := range rounds {
				for size, rtt := range map[int]func(int) time.Duration{pathProbeSmall: p.small, pathProbeLarge: large} {
					s := PathSample{Via: p.via, Size: size, Started: start.Add(time.Duration(i) * every),
						NextHop: addr[hop], LocalPort: port, Epoch: epoch, RTT: rtt(i)}
					if s.RTT == 0 {
						s.Err = context.DeadlineExceeded
					}
					if p.err != nil && i%3 == 0 {
						s = PathSample{Via: p.via, Size: size, Started: s.Started, Err: p.err}
					}
					out = append(out, s)
				}
			}
		}
		return out
	}

	direct80 := path{small: ms(80)}
	for _, tc := range []struct {
		name       string
		samples    []PathSample
		sufficient bool
		via        Key
		reject     string // 中转（Paths[1]）的拒绝理由
	}{
		{"faster relay wins", build(direct80, path{via: pooled, small: ms(40)}), true, pooled, ""},
		// 一个延迟低一点点的中转（75 对 80，差距不到 10 ms）不值得换。
		{"marginal gain keeps direct", build(direct80, path{via: pooled, small: ms(75)}), true, Key{}, "没有比直连明显更好"},
		// 1/10 轮丢包 = 10%，超过直连 0% 加 2 个百分点。
		{"lossy relay rejected", build(direct80, path{via: pooled, small: lossEvery(40, 10)}), true, Key{}, "比直连多丢包"},
		// 小包很好、大包全丢：典型的 MTU 黑洞，不能被低延迟掩盖。
		{"large packet black hole", build(direct80, path{via: pooled, small: ms(40), large: ms(0)}), true, Key{}, "大包比直连多丢包（可能是 MTU 不够）"},
		// 中位数 40 ms，但每 5 轮一次 300 ms：90 分位 300 ms 比直连 80 ms 差，不稳定。
		{"unstable relay rejected", build(direct80, path{via: pooled, small: func(i int) time.Duration {
			if i%5 == 0 {
				return 300 * time.Millisecond
			}
			return 40 * time.Millisecond
		}}), true, Key{}, "没有比直连明显更好"},
		// 直连 1/5 轮丢包（20%），中转不丢、延迟高 5 ms（在 12 ms 容差内）：更稳就选它。
		{"steadier relay on lossy direct", build(path{small: lossEvery(80, 5)}, path{via: pooled, small: ms(85)}), true, pooled, ""},
		// 本地预算不足不算丢包：1/3 轮被跳过，剩下 40 轮仍足够，中转照样入选。
		{"skipped attempts are not loss", build(direct80, path{via: pooled, small: ms(40), err: ErrProbeLimited}), true, pooled, ""},
		{"relay outside pool", build(direct80, path{via: outside, small: ms(20)}), true, Key{}, "不在中转池"},
		{"relay hop changed", build(direct80, path{via: pooled, small: ms(20), epoch: 2}), true, Key{}, "第一跳在窗口里变过或已经失效，要重新观察"},
		{"relay unauthorized", build(direct80, path{via: pooled, small: ms(20), err: ErrProbePath}), true, Key{}, "路线不可用或没有授权"},
		// 只看到 20 秒。
		{"window too short", build(path{small: ms(80), roundEvery: time.Second / 3}, path{via: pooled, small: ms(20), roundEvery: time.Second / 3}), false, Key{}, ""},
		// 60 秒里只有 20 轮：每条路线每种包长 20 次，少于 25 次。
		{"too few samples", build(path{small: ms(80), rounds: 20, roundEvery: 3 * time.Second}, path{via: pooled, small: ms(20), rounds: 20, roundEvery: 3 * time.Second}), false, Key{}, ""},
		// 直连的第一跳变过：直连没有可信基线，不给建议，也不能当成「该回直连」。
		{"direct hop changed", build(path{small: ms(80), epoch: 2}, path{via: pooled, small: ms(20)}), false, Key{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.ComparePaths(peer, tc.samples)
			if got.Sufficient != tc.sufficient || got.Via != tc.via || got.Reason == "" {
				t.Fatalf("advice = %+v", got)
			}
			if len(got.Paths) != 2 || got.Paths[0].Via != (Key{}) {
				t.Fatalf("paths = %+v", got.Paths)
			}
			if tc.sufficient && got.Paths[1].Reject != tc.reject {
				t.Fatalf("relay reject = %q, want %q", got.Paths[1].Reject, tc.reject)
			}
		})
	}

	got := m.ComparePaths(peer, build(direct80, path{via: pooled, small: ms(40), err: ErrProbeLimited}))
	if r := got.Paths[1].Small; r.Measured != 40 || r.Lost != 0 || r.Skipped != 20 || r.P50 != 40*time.Millisecond {
		t.Fatalf("skipped attempts miscounted: %+v", r)
	}
	got = m.ComparePaths(peer, build(direct80, path{via: pooled, small: func(i int) time.Duration { return time.Duration(i+1) * time.Millisecond }}))
	if r := got.Paths[1].Small; r.P50 != 30*time.Millisecond || r.P90 != 54*time.Millisecond {
		t.Fatalf("percentiles of 1..60 ms: %+v", r)
	}
	// 退出中转池以后，同一组样本不再给出经它的建议。
	b.setPeers([]Peer{{Key: peer}, {Key: pooled, NodeRelayEnabled: &on}})
	for _, k := range []Key{peer, pooled} {
		p := b.peers[k]
		p.direct, p.directAt, p.directEpoch = addr[k], time.Now(), 3
	}
	got = m.ComparePaths(peer, build(direct80, path{via: pooled, small: ms(40)}))
	if got.Via != (Key{}) || got.Paths[1].Reject != "不在中转池" {
		t.Fatalf("left pool but still advised: %+v", got)
	}
}
