package mesh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestRelayAuthorizationAndPin(t *testing.T) {
	t.Parallel()
	r, a, b := newNode(t, false), newNode(t, false), newNode(t, false)
	r.m.SetNodeRelay(true)
	rk, ak, bk := r.m.PublicKey(), a.m.PublicKey(), b.m.PublicKey()
	on, off := true, false
	set := func(n testNode, other testNode, candidates []Key, permission *bool) {
		t.Helper()
		if err := n.m.SetPeers([]Peer{
			{Key: rk, Endpoints: local(r.port), NodeRelayEnabled: permission},
			{Key: other.m.PublicKey(), Endpoints: local(other.port), NodeRelays: candidates},
		}); err != nil {
			t.Fatal(err)
		}
	}
	dropFor(t, func(from uint16, to netip.AddrPort) bool {
		return (from == a.port && to.Port() == b.port) || (from == b.port && to.Port() == a.port)
	})
	if err := r.m.SetPeers([]Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}}); err != nil {
		t.Fatal(err)
	}
	set(a, b, []Key{rk}, &on)
	set(b, a, []Key{rk}, &on)
	for _, pair := range []struct{ from, to testNode }{{a, r}, {b, r}, {r, a}, {r, b}} {
		waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
	}
	echo(t, a, b)
	before := r.m.bind.relayRx.Load()
	// 两端候选都刷新为空，但独立许可仍在；已有路径和双向收包不能跟着消失。
	set(a, b, nil, &on)
	set(b, a, nil, &on)
	waitPath(t, a.m, bk, "node-relay")
	echo(t, a, b)
	if r.m.bind.relayRx.Load() <= before {
		t.Fatal("candidate refresh stopped the active relay")
	}
	// 有独立许可时，接收不依赖本方向有没有候选；固定观测仍经 R。
	if s := a.m.ProbePath(context.Background(), bk, rk, 1180); s.Err != nil {
		t.Fatalf("authorized pinned route: %+v", s)
	}
	// 即使旧候选仍列着 R，显式撤权也立即生效。
	set(a, b, []Key{rk}, &off)
	waitPath(t, a.m, bk, "none")
	if s := a.m.ProbePath(context.Background(), bk, rk, 256); !errors.Is(s.Err, ErrProbePath) {
		t.Fatalf("explicit revocation ignored: %+v", s)
	}
	// 回到旧面板契约：当前候选可用，但候选消失不能延长旧许可。
	set(a, b, []Key{rk}, nil)
	waitPath(t, a.m, bk, "node-relay")
	echo(t, a, b)
	set(a, b, nil, nil)
	waitPath(t, a.m, bk, "none")
	// 删除成员也必须撤掉此前钉住的路径。
	set(a, b, []Key{rk}, &on)
	echo(t, a, b)
	if err := a.m.SetPeers([]Peer{{Key: bk, NodeRelays: []Key{rk}}}); err != nil {
		t.Fatal(err)
	}
	waitPath(t, a.m, bk, "none")
}

func TestPathProbeFixedRoute(t *testing.T) {
	t.Parallel()
	r, a, b := newNode(t, false), newNode(t, false), newNode(t, false)
	r.m.SetNodeRelay(true)
	rk, ak, bk := r.m.PublicKey(), a.m.PublicKey(), b.m.PublicKey()
	set := func(n testNode, peers []Peer) {
		t.Helper()
		if err := n.m.SetPeers(peers); err != nil {
			t.Fatal(err)
		}
	}
	set(r, []Peer{{Key: ak, Endpoints: local(a.port)}, {Key: bk, Endpoints: local(b.port)}})
	set(a, []Peer{{Key: rk, Endpoints: local(r.port)}, {Key: bk, Endpoints: local(b.port), NodeRelays: []Key{rk}}})
	set(b, []Peer{{Key: rk, Endpoints: local(r.port)}, {Key: ak, Endpoints: local(a.port), NodeRelays: []Key{rk}}})
	for _, pair := range []struct{ from, to testNode }{{a, b}, {a, r}, {b, a}, {b, r}, {r, a}, {r, b}} {
		waitPath(t, pair.from.m, pair.to.m.PublicKey(), "direct")
	}
	a.m.bind.mu.Lock()
	healthBefore := map[Key]relayHealth{}
	for k, h := range a.m.bind.peers[bk].relayHealth {
		healthBefore[k] = *h
	}
	a.m.bind.mu.Unlock()
	rx, tx := r.m.bind.relayRx.Load(), r.m.bind.relayTx.Load()
	for _, pair := range []struct{ from, to testNode }{{a, b}, {b, a}} {
		for _, via := range []Key{{}, rk} {
			for _, size := range []int{256, 1180} {
				s := pair.from.m.ProbePath(context.Background(), pair.to.m.PublicKey(), via, size)
				// 本机 UDP 往返可能小于系统时钟分辨率；成功由认证后的 Err == nil 区分。
				if s.Err != nil || s.RTT < 0 || s.Size != size || s.Epoch == 0 || s.LocalPort != pair.from.port {
					t.Fatalf("invalid sample: %+v", s)
				}
				want := pair.to.port
				if via == rk {
					want = r.port
				}
				if s.NextHop.Port() != want {
					t.Fatalf("next hop: %v, want port %d", s.NextHop, want)
				}
			}
		}
	}
	// 每方向每尺寸经 R 转发两次。独立转发计数证明不是偷偷测直连。
	if gotRx, gotTx := r.m.bind.relayRx.Load()-rx, r.m.bind.relayTx.Load()-tx; gotRx != 5744 || gotTx != 5744 {
		t.Fatalf("R forwarded rx=%d tx=%d, want 5744 each", gotRx, gotTx)
	}
	a.m.bind.mu.Lock()
	p := a.m.bind.peers[bk]
	_, polluted := p.learned[local(r.port)[0]]
	healthAfter := map[Key]relayHealth{}
	for k, h := range p.relayHealth {
		healthAfter[k] = *h
	}
	unchanged := p.direct == local(b.port)[0] && maps.Equal(healthBefore, healthAfter)
	a.m.bind.mu.Unlock()
	if polluted || !unchanged {
		t.Fatalf("probe polluted direct/NAT/business relay health: NAT=%t before=%+v after=%+v", polluted, healthBefore, healthAfter)
	}
	// R 不转发，但直连仍通。指定 R 必须超时，不能回落到直连或面板。
	r.m.SetNodeRelay(false)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	s := a.m.ProbePath(ctx, bk, rk, 1180)
	cancel()
	if !errors.Is(s.Err, context.DeadlineExceeded) || s.RTT != 0 {
		t.Fatalf("dead R should time out: %+v", s)
	}
	if s := a.m.ProbePath(context.Background(), bk, Key{}, 256); s.Err != nil {
		t.Fatalf("direct should still work: %+v", s)
	}
	// 收端撤销候选仍须拒收，不能为维持样本擅自保留授权。
	r.m.SetNodeRelay(true)
	set(b, []Peer{{Key: rk, Endpoints: local(r.port)}, {Key: ak, Endpoints: local(a.port)}})
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	s = a.m.ProbePath(ctx, bk, rk, 256)
	cancel()
	if !errors.Is(s.Err, context.DeadlineExceeded) {
		t.Fatalf("revoked relay accepted: %+v", s)
	}
	a.m.bind.mu.Lock()
	a.m.bind.probeBudget = relayBucket{at: time.Now()}
	left := len(a.m.bind.pathProbes)
	a.m.bind.mu.Unlock()
	if left != 0 {
		t.Fatalf("completed/timed-out probes leaked: %d", left)
	}
	if s := a.m.ProbePath(context.Background(), bk, Key{}, 1180); !errors.Is(s.Err, ErrProbeLimited) {
		t.Fatalf("origin budget ignored: %+v", s)
	}
	waitPath(t, a.m, bk, "direct")
	echo(t, a, b) // 业务未被观测切路，现有 QUIC/WG 仍可收发。
}

func TestPathProbeResponseValidation(t *testing.T) {
	// 所有错误应答先到，真正应答随后到；错误不能消费事务或生成成功样本。
	for _, attack := range []string{"mac", "relay", "transaction", "size", "socket", "expired", "epoch", "removed-peer", "request-reflection"} {
		t.Run(attack, func(t *testing.T) {
			b := newBind(Key{8}, false, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			peer := PublicKey(Key{16})
			b.setPeers([]Peer{{Key: peer}})
			p := b.peers[peer]
			from := netip.MustParseAddrPort("127.0.0.1:12345")
			p.direct, p.directAt = from, time.Now()
			p.confirmed[from], b.byAddr[from] = time.Now(), peer
			id := [txidLen]byte{1, 7, 9}
			pending := &pathProbe{peer: p, hop: p, addr: from, size: 256, sent: time.Now().Add(-time.Millisecond),
				deadline: time.Now().Add(time.Second), result: make(chan time.Duration, 1)}
			b.pathProbes = map[[txidLen]byte]*pathProbe{id: pending}
			pkt := encodePathProbe(typePathResponse, peer, b.pub, Key{}, id, 256, p.mac)
			local := 0
			switch attack {
			case "mac":
				pkt[len(pkt)-1] ^= 1
			case "relay":
				pkt = encodePathProbe(typePathResponse, peer, b.pub, PublicKey(Key{24}), id, 256, p.mac)
			case "transaction":
				pkt = encodePathProbe(typePathResponse, peer, b.pub, Key{}, [txidLen]byte{2}, 256, p.mac)
			case "size":
				pkt = encodePathProbe(typePathResponse, peer, b.pub, Key{}, id, 1180, p.mac)
			case "socket":
				local = 1
			case "expired":
				pending.deadline = time.Now().Add(-time.Second)
			case "epoch":
				p.directEpoch++
			case "removed-peer":
				b.setPeers(nil)
				b.setPeers([]Peer{{Key: peer}})
				fresh := b.peers[peer]
				fresh.direct, fresh.directAt = from, time.Now()
				fresh.confirmed[from], b.byAddr[from] = time.Now(), peer
			case "request-reflection":
				pkt[4] = typePathRequest // 请求/响应的认证用途不能互换。
			}
			b.handleControl(nil, local, from, pkt)
			select {
			case <-pending.result:
				t.Fatal("invalid response generated sample")
			default:
			}
			// 恢复同一活动事务后只接受一次正确应答，随后重放无效。
			b.peers[peer] = p
			b.byAddr[from] = peer
			p.directEpoch = 0
			pending.deadline = time.Now().Add(time.Second)
			valid := encodePathProbe(typePathResponse, peer, b.pub, Key{}, id, 256, p.mac)
			b.handleControl(nil, 0, from, valid)
			select {
			case rtt := <-pending.result:
				if rtt <= 0 {
					t.Fatal("nonpositive RTT")
				}
			default:
				t.Fatal("valid response rejected")
			}
			b.handleControl(nil, 0, from, valid)
			select {
			case <-pending.result:
				t.Fatal("replay generated second sample")
			default:
			}
		})
	}
}

func TestObservePathsRetainsFailures(t *testing.T) {
	t.Parallel()
	_, a, b := setup(t)
	waitPath(t, a.m, b.m.PublicKey(), "direct")
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	// 一个非候选中转：每轮直连成功、指定路径明确失败，不能被丢出结果。
	samples, err := a.m.ObservePaths(ctx, b.m.PublicKey(), []Key{PublicKey(Key{56})})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	var successes, failures int
	for _, s := range samples {
		if s.Via == (Key{}) && s.Err == nil && s.RTT >= 0 {
			successes++
		}
		if s.Via != (Key{}) && errors.Is(s.Err, ErrProbePath) {
			failures++
		}
	}
	if successes < 2 || failures < 2 || successes+failures != len(samples) {
		t.Fatalf("samples lost or mislabeled: %+v", samples)
	}
	if _, err := a.m.ObservePaths(ctx, b.m.PublicKey(), []Key{{}, {}}); !errors.Is(err, ErrProbePath) {
		t.Fatal("duplicate/direct relay accepted")
	}
}

func TestPathProbeRequestReplayAndBudget(t *testing.T) {
	b := newBind(Key{8}, false, 0, 0, slog.Default())
	peer := PublicKey(Key{16})
	b.setPeers([]Peer{{Key: peer}})
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	from := r.LocalAddr().(*net.UDPAddr).AddrPort()
	p := b.peers[peer]
	p.direct, p.directAt = from, time.Now()
	p.confirmed[from], b.byAddr[from] = time.Now(), peer
	pkt := encodePathProbe(typePathRequest, peer, b.pub, Key{}, [txidLen]byte{9}, 1180, p.mac)
	b.handleControl(c, 0, from, pkt)
	buf := make([]byte, 1500)
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := r.ReadFromUDP(buf)
	if err != nil || n != 1180 || buf[4] != typePathResponse || !verify(buf[:n], p.mac) {
		t.Fatalf("incorrect authenticated equal-size reply: n=%d err=%v", n, err)
	}
	b.handleControl(c, 0, from, pkt)
	_ = r.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := r.ReadFromUDP(buf); err == nil {
		t.Fatal("replayed request answered")
	}
	b.probeBudget = relayBucket{at: time.Now()} // 没有剩余预算，禁止响应。
	pkt = encodePathProbe(typePathRequest, peer, b.pub, Key{}, [txidLen]byte{10}, 1180, p.mac)
	b.handleControl(c, 0, from, pkt)
	_ = r.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := r.ReadFromUDP(buf); err == nil {
		t.Fatal("budget ignored")
	}
}
