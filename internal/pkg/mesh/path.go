package mesh

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	pathProbeSmall = 256
	pathProbeLarge = routedQUICMTU + headerLen + macLen // 1253：与实验 QUIC 等长，经 R 的 IPv6 外层共 1338 字节。
	pathProbeTTL   = 3 * time.Second
	maxPathProbes  = 32
	probeRate      = 32 << 10 // 每个节点总预算（收发合计），不随成员数量增长。
	probeBurst     = 64 << 10
)

var (
	ErrProbeLimited = errors.New("mesh 路径探测预算不足")
	ErrProbePath    = errors.New("mesh 指定探测路径不可用或未获授权")
)

type pathProbe struct {
	peer     *peerState
	hop      *peerState
	via      Key
	addr     netip.AddrPort
	local    int
	size     int
	sent     time.Time
	deadline time.Time
	epoch    uint64
	result   chan PathSample
}

// PathSample 只表示一次固定路径往返，不代表业务验证或单向丢包率。
// Via 为零是直连；Size 是未加中转头的 UDP 载荷长度。Err 非空的尝试也必须保留。
type PathSample struct {
	Via     Key
	Size    int
	Started time.Time
	// NextHop、LocalPort、Epoch 标识本地第一跳，不能混合不同代次的样本凑观察窗口。
	// 尚不包含中转另一段或远端启动代次，不能直接用作自动晋升证据。
	NextHop   netip.AddrPort
	LocalPort uint16
	Epoch     uint64
	RTT       time.Duration
	Err       error
}

// probeVia 判断能不能经 via 测到 p：面板给的故障兜底候选、正在用的中转，或者中转池里的节点（用来给直连可用的节点对
// 找性能更好的路线）。最后一种只在 via 加入了中转池、有显式许可时才行，不把「愿意故障中转」当成同意性能中转。
// 调用时持有 b.mu。
func (b *Bind) probeVia(p *peerState, via Key) bool {
	if !b.relayKeys[via] {
		return false
	}
	return p.activeRelay == via || p.quicVia == via || slices.Contains(p.nodeRelays, via) ||
		slices.Contains(p.relays, via) || b.peers[via].poolRelay()
}

func validProbeSize(size int) bool {
	return size == pathProbeSmall || size == 1180 || size == pathProbeLarge
}

// ProbePath 显式测量直连或经指定中转的完整往返，不改变业务路径，不自动兜底。
// size 只接受 256、1180（兼容早期观察协议）或 1253。每次最多等 3 秒，调用者可用 ctx 进一步缩短。
// 双端都须支持新探测；旧端点不响应，返回超时，不会被视为低延迟。
func (m *Mesh) ProbePath(ctx context.Context, peer, via Key, size int) (sample PathSample) {
	sample = PathSample{Via: via, Size: size, Started: time.Now()}
	if err := ctx.Err(); err != nil {
		sample.Err = err
		return
	}
	if !validProbeSize(size) {
		sample.Err = errors.New("mesh 探测大小只能为 256、1180 或 1253")
		return
	}
	b := m.bind
	now := time.Now()
	b.mu.Lock()
	p := b.peers[peer]
	hop := p
	if p == nil || peer == b.pub || via == peer || via == b.pub {
		b.mu.Unlock()
		sample.Err = ErrProbePath
		return
	}
	if via != (Key{}) {
		if !b.probeVia(p, via) {
			b.mu.Unlock()
			sample.Err = ErrProbePath
			return
		}
		hop = b.peers[via]
	}
	if hop == nil || !hop.directAlive(now) || b.sock(hop.directConn) == nil {
		b.mu.Unlock()
		sample.Err = ErrProbePath
		return
	}
	if len(b.pathProbes) >= maxPathProbes || !b.allowProbe(2*(size+headerLen), now) {
		b.mu.Unlock()
		sample.Err = ErrProbeLimited
		return
	}
	var id [txidLen]byte
	_, _ = rand.Read(id[:])
	deadline := now.Add(pathProbeTTL)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	sample.NextHop, sample.Epoch = hop.direct, hop.directEpoch
	sample.LocalPort = uint16(b.sock(hop.directConn).LocalAddr().(*net.UDPAddr).Port)
	pending := &pathProbe{peer: p, hop: hop, via: via, addr: hop.direct, local: hop.directConn, epoch: hop.directEpoch,
		size: size, sent: now, deadline: deadline, result: make(chan PathSample, 1)}
	if b.pathProbes == nil {
		b.pathProbes = make(map[[txidLen]byte]*pathProbe)
	}
	b.pathProbes[id] = pending
	pkt := encodePathProbe(typePathRequest, b.pub, peer, via, id, size, p.mac)
	c, done := b.sock(hop.directConn), b.done
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pathProbes, id)
		b.mu.Unlock()
	}()
	if via != (Key{}) {
		pkt = encodeRelay(typeRelaySend, peer, pkt)
	}
	b.write(c, pkt, pending.addr)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case result := <-pending.result:
		sample.RTT, sample.Err = result.RTT, result.Err
	case <-ctx.Done():
		sample.Err = ctx.Err()
	case <-timer.C:
		sample.Err = context.DeadlineExceeded
	case <-done:
		sample.Err = net.ErrClosed
	}
	return
}

// ObservePaths 显式进行一个完整 60 秒观察窗口，每秒交错测直连及最多两个中转的两种包长。
// 无后台调度、无生产流量复制、无晋升或切路；中途取消时仍返回已完成的全部尝试。结果交给 ComparePaths 比较。
// 每节点同一时间仅允许一个窗口，重叠调用返回 ErrProbeLimited；关闭 mesh 时及时停止。
// 地址、socket 或候选授权改变时失败而非混入别的路径；后续策略须据失败/新鲜度重新观察。
func (m *Mesh) ObservePaths(ctx context.Context, peer Key, relays []Key) ([]PathSample, error) {
	if len(relays) > 2 || slices.Contains(relays, Key{}) || (len(relays) == 2 && relays[0] == relays[1]) {
		return nil, ErrProbePath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	select {
	case <-m.done:
		m.mu.Unlock()
		return nil, net.ErrClosed
	default:
	}
	if m.observing {
		m.mu.Unlock()
		return nil, ErrProbeLimited
	}
	m.observing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.observing = false
		m.mu.Unlock()
	}()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	paths := append([]Key{{}}, relays...)
	var samples []PathSample
	var mu sync.Mutex
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		var wg sync.WaitGroup
		for i, via := range paths {
			for j, size := range []int{pathProbeSmall, pathProbeLarge} {
				wg.Go(func() {
					// 同期交错大小包和不同路径，并略微错峰；不是一次向所有下一跳突发。
					var jitter [1]byte
					_, _ = rand.Read(jitter[:])
					delay := time.NewTimer(time.Duration((i*2+j)*30+int(jitter[0]&15)) * time.Millisecond)
					defer delay.Stop()
					select {
					case <-ctx.Done():
						return
					case <-m.done:
						return
					case <-delay.C:
					}
					// 窗口到点只停止发新的一轮，已经发出的等它自己的 800 ms，免得窗口末尾被截断的尝试记成丢包。
					// 调用者取消或到期打断的那次尝试不记样本：它不是链路结果。
					attempt, stop := context.WithTimeout(parent, 800*time.Millisecond)
					defer stop()
					s := m.ProbePath(attempt, peer, via, size)
					if s.Err != nil && parent.Err() != nil {
						return
					}
					mu.Lock()
					samples = append(samples, s)
					mu.Unlock()
				})
			}
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return samples, parent.Err()
		case <-m.done:
			return samples, net.ErrClosed
		case <-tick.C:
		}
	}
	return samples, parent.Err()
}

// 请求/响应分别用 type 区分认证用途。目的端、指定中转、事务号、包长和填充都受 MAC 保护。
func encodePathProbe(typ byte, self, target, via Key, id [txidLen]byte, size int, mac [32]byte) []byte {
	pkt := header(typ, self)
	pkt = append(pkt, target[:]...)
	pkt = append(pkt, via[:]...)
	pkt = append(pkt, id[:]...)
	pkt = append(pkt, make([]byte, size-macLen-len(pkt))...)
	return append(pkt, sum(mac, pkt)...)
}

// allowProbe 预留一来一回的字节预算，调用时持有 b.mu。
func (b *Bind) allowProbe(n int, now time.Time) bool {
	bk := &b.probeBudget
	if bk.at.IsZero() {
		bk.tokens = probeBurst
	} else {
		bk.tokens = min(probeBurst, bk.tokens+now.Sub(bk.at).Seconds()*probeRate)
	}
	bk.at = now
	if bk.tokens < float64(n) {
		return false
	}
	bk.tokens -= float64(n)
	return true
}

func (b *Bind) handlePathProbe(c *net.UDPConn, local int, from netip.AddrPort, relay Key, pkt []byte) {
	m, ok := parse(pkt)
	if !ok || (m.typ != typePathRequest && m.typ != typePathResponse) {
		return
	}
	var target, via Key
	var id [txidLen]byte
	copy(target[:], m.payload[:32])
	copy(via[:], m.payload[32:64])
	copy(id[:], m.payload[64:64+txidLen])
	if target != b.pub || via != relay {
		return
	}
	now := time.Now()
	b.mu.Lock()
	p := b.peers[m.key]
	if p == nil || !verify(pkt, p.mac) {
		b.mu.Unlock()
		return
	}
	hop := p
	if relay != (Key{}) {
		if !b.relayKeys[relay] {
			b.mu.Unlock()
			return
		}
		hop = b.peers[relay]
	}
	if hop == nil || !hop.directAlive(now) || b.byAddr[from] != hop.key || now.Sub(hop.confirmed[from]) >= directTimeout {
		b.mu.Unlock()
		return
	}
	if m.typ == typePathResponse {
		pending := b.pathProbes[id]
		if pending != nil && pending.peer == p && pending.hop == hop && pending.via == via &&
			pending.addr == from && pending.local == local && pending.size == len(pkt) && now.Before(pending.deadline) &&
			hop.direct == pending.addr && hop.directConn == pending.local && hop.directEpoch == pending.epoch {
			delete(b.pathProbes, id) // 消费一次，重放不能再产生样本。
			pending.result <- PathSample{RTT: now.Sub(pending.sent)}
		}
		b.mu.Unlock()
		return
	}
	if !b.allowProbe(2*(len(pkt)+headerLen), now) {
		b.mu.Unlock()
		return
	}
	if b.probeSeen == nil {
		b.probeSeen = make(map[Key]map[[txidLen]byte]time.Time)
	}
	seen := b.probeSeen[m.key]
	if seen == nil {
		seen = make(map[[txidLen]byte]time.Time)
		b.probeSeen[m.key] = seen
	}
	for txid, at := range seen {
		if now.Sub(at) > pathProbeTTL {
			delete(seen, txid)
		}
	}
	if _, replay := seen[id]; replay || len(seen) >= 128 {
		b.mu.Unlock()
		return
	}
	seen[id] = now
	response := encodePathProbe(typePathResponse, b.pub, m.key, via, id, len(pkt), p.mac)
	b.mu.Unlock()
	if relay != (Key{}) {
		response = encodeRelay(typeRelaySend, m.key, response)
	}
	// 固定从接收 socket 回复同一物理下一跳，绝不调用 sendTo 或转入面板兜底。
	b.write(c, response, from)
}
