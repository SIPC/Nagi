package mesh

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// Link 经控制连接（TLS）发送一个 WireGuard 包。src、dst 是原始发送方和最终接收方。
type Link func(src, dst Key, payload []byte) error

const (
	directTimeout = 25 * time.Second // 这么久没收到 pong 就认为直连断了
	pingDirect    = 10 * time.Second // 直连可用时的保活间隔
	pingSearch    = 5 * time.Second  // 直连不可用时探测候选地址的间隔
	forget        = 2 * time.Minute  // 学到的地址、看到的自己的公网地址保留多久
	maxCandidates = 16
	// pendingInitTTL 是没有路径时暂存的握手包留多久：再久 WireGuard 自己就重发了（REKEY_TIMEOUT 是 5 秒）。
	pendingInitTTL = 5 * time.Second
	// capsTimeout：这么久没收到对方的能力包，就当它不支持（例如换回了旧版本）。
	capsTimeout = 45 * time.Second
)

// msgInitiation 是 WireGuard 握手发起包的类型（第一个字节）。
const msgInitiation = 1

// dropUDP 只用于测试：返回 true 时丢弃这个 UDP 包，模拟网络不通。
var dropUDP func(fromPort uint16, to netip.AddrPort) bool

// endpoint 是 WireGuard 眼里的对端地址。这里它就是对端本身（公钥），走哪条路径由 Bind 决定。
// 同一个对端始终是同一个 endpoint，所以 WireGuard 的漫游不会把它换掉。
type endpoint struct{ key Key }

func (e *endpoint) ClearSrc()           {}
func (e *endpoint) SrcToString() string { return "" }
func (e *endpoint) DstToString() string { return e.key.String() }
func (e *endpoint) DstToBytes() []byte  { return e.key[:] }
func (e *endpoint) DstIP() netip.Addr   { return Address(e.key) }
func (e *endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

type pingInfo struct {
	addr  netip.AddrPort
	local int // 从哪个本地端口发出的（Bind.conns 的下标）
	sent  time.Time
}

// pingKey 是「从哪个本地端口、发往哪个地址」：同一个地址从不同的本地端口探测，分开计时。
type pingKey struct {
	local int
	addr  netip.AddrPort
}

type peerState struct {
	key        Key
	ep         *endpoint
	mac        [32]byte
	candidates []netip.AddrPort             // 别人告诉我们的地址
	learned    map[netip.AddrPort]time.Time // 从对方的 ping 学到的地址
	lastPing   map[pingKey]time.Time        // 每个本地端口、每个地址上次发 ping 的时间
	pings      map[[txidLen]byte]pingInfo   // 等待 pong 的 ping
	direct     netip.AddrPort               // 确认能直连的地址
	directConn int                          // 直连用的本地端口（Bind.conns 的下标）
	// confirmed 是最近经 ping/pong 确认过的对方地址（对方有备用端口时可能不止一个）。这些地址发来的包都收，
	// 发只走 direct：两端各自选端口，对方用来发的端口不一定是我们选来发给它的那个。
	confirmed map[netip.AddrPort]time.Time
	directAt   time.Time                    // 最近一次 pong 的时间
	rtt        time.Duration
	relays     []Key // 可以代为中转的面板
	nodeRelays []Key // 可以代为中转的节点，先于面板用
	// relayHealth：经每个中转节点发给这个对端以后，有没有经它收到对端的包（见 pickNodeRelay）。
	relayHealth map[Key]*relayHealth
	// pendingInit 是没有路径时发不出去的握手发起包，路径一可用就补发。例如对端刚重启、直连还没确认：
	// 不补发的话要等 WireGuard 5 秒后重发，这期间对方还用着重启前的会话，发来的包都被丢掉。
	pendingInit   []byte
	pendingInitAt time.Time

	caps    byte // 对方的能力（capFEC、capQUIC），capsAt 之后 capsTimeout 内有效
	capsAt  time.Time
	lossPPM uint32 // 对方报告的、它收到我们的包的丢包率
	lossAt  time.Time
	// rxLoss：上一次算出的、我们收到对方的包的丢包率（经 WireGuard 的，按 FEC 序号算）。
	rxLoss   uint32
	rxLossAt time.Time
	enc      *fecEncoder
	dec      *fecDecoder
	// quicRx、quicTx：和对方之间收发的 QUIC 包的字节数。QUIC 不经过 WireGuard，要另外算进流量里。
	quicRx, quicTx uint64
}

func (p *peerState) has(c byte, now time.Time) bool {
	return p.caps&c != 0 && now.Sub(p.capsAt) < capsTimeout
}

func (p *peerState) directAlive(now time.Time) bool {
	return p.direct.IsValid() && now.Sub(p.directAt) < directTimeout
}

type delivery struct {
	ep   *endpoint
	data []byte
}

// Bind 是 wireguard-go 的收发层，实现直连、UDP 中转和 TLS 中转。
type Bind struct {
	priv, pub Key
	relay     bool // 是否替别人中转（面板）
	// nodeRelay：节点替别的节点中转（网络设置里可以关）。只经直连的 UDP 转发，不经控制连接，按来源限速。
	nodeRelay atomic.Bool
	// relayRx、relayTx：替别人中转收到、转出的字节数（面板），不属于任何一个对端，Status 里没有。
	relayRx, relayTx atomic.Uint64
	caps             byte // 自己启用的能力
	log              *slog.Logger

	// extra 是主端口以外再开的 UDP 端口数。线路按「源地址、源端口、目的地址、目的端口」做负载均衡、
	// 其中一条链路坏了时，落在它上面的流一个包也过不去，换一个源端口就可能落到好的链路上：
	// 直连不通时从每个端口都探测一遍，哪个端口先收到回复就和这个对端一直用它（见 docs/mesh.md#路径选择）。
	// 备用端口只用来往外发，对方的回复是同一条流的回包，所以不用固定端口，也不用在防火墙上放行。
	extra int

	mu        sync.Mutex
	conns     []*net.UDPConn // 第一个是主端口，其余是备用端口
	port      uint16
	peers     map[Key]*peerState
	byAddr    map[netip.AddrPort]Key // 已确认的直连地址 → 对端
	byMesh    map[netip.Addr]Key     // mesh 地址 → 对端
	relayKeys map[Key]bool
	links     map[Key]*Link
	quic      *quicPacketConn              // 收到的 QUIC 包交给它（见 quic.go）
	onCaps    func(Key)                    // 对方刚表示支持 QUIC 时调用
	observed  map[pingKey]time.Time // 别人看到的我们的地址，按本地端口分开（local 是本地端口的下标）
	recv      chan delivery
	done      chan struct{}
	// relayBudget：节点替每个来源中转的字节数配额（令牌桶，见 relayAllowed）。
	relayBudget map[Key]*relayBucket
}

// relayBucket 是一个来源的中转配额。
type relayBucket struct {
	tokens float64
	at     time.Time
}

const (
	// 节点替一个来源中转的速率上限和突发量：够两个节点之间正常用，又不让一个被攻破的节点借中转刷爆别人的带宽。
	relayRate  = 32 << 20 // 字节/秒
	relayBurst = 8 << 20
)

// relayHealth 是经某个中转节点到某个对端通不通。中转节点关了中转、或者和对端断了直连，发过去的包会被它丢掉，
// 我们这边看不出来：发了以后 relayProbe 内没有经它收到对端的包，就当它不通，relayBad 内不用它，之后再试
// （例如刚开始时它还没确认到对端的直连）。
// 两边都没有流量时不判断，所以空闲时不会来回换。
type relayHealth struct {
	waitingSince time.Time // 经它发了、还没收到回包的起点
	lastRecv     time.Time
	badUntil     time.Time
}

const (
	relayProbe = 10 * time.Second
	relayBad   = 30 * time.Second
)

// pickNodeRelay 返回发给 p 用的中转节点，没有可用的时 ok 为 false。sending 为 true 时记下经它发了包。调用时持有 b.mu。
func (b *Bind) pickNodeRelay(p *peerState, now time.Time, sending bool) (r Key, rp *peerState, ok bool) {
	for _, r := range p.nodeRelays {
		rp := b.peers[r]
		if rp == nil || !rp.directAlive(now) {
			continue
		}
		if p.relayHealth == nil {
			p.relayHealth = map[Key]*relayHealth{}
		}
		h := p.relayHealth[r]
		if h == nil {
			h = &relayHealth{}
			p.relayHealth[r] = h
		}
		if now.Before(h.badUntil) {
			continue
		}
		if !h.waitingSince.IsZero() && now.Sub(h.waitingSince) > relayProbe {
			h.waitingSince, h.badUntil = time.Time{}, now.Add(relayBad)
			b.log.Debug("mesh 中转节点不通，暂时不用", "peer", p.key.Short(), "relay", r.Short())
			continue
		}
		if sending && h.waitingSince.IsZero() {
			h.waitingSince = now
		}
		return r, rp, true
	}
	return Key{}, nil, false
}

// relayHeard 记下经中转节点 r 收到了 from 的包。调用时持有 b.mu。
func (b *Bind) relayHeard(from *peerState, r Key, now time.Time) {
	if h := from.relayHealth[r]; h != nil {
		h.waitingSince, h.lastRecv, h.badUntil = time.Time{}, now, time.Time{}
	}
}

// relayAllowed 扣掉 src 的 n 字节中转配额，不够时返回 false。调用时持有 b.mu。
func (b *Bind) relayAllowed(src Key, n int, now time.Time) bool {
	if b.relayBudget == nil {
		b.relayBudget = map[Key]*relayBucket{}
	}
	bk := b.relayBudget[src]
	if bk == nil {
		bk = &relayBucket{tokens: relayBurst, at: now}
		b.relayBudget[src] = bk
	}
	bk.tokens = min(relayBurst, bk.tokens+now.Sub(bk.at).Seconds()*relayRate)
	bk.at = now
	if bk.tokens < float64(n) {
		return false
	}
	bk.tokens -= float64(n)
	return true
}

func newBind(priv Key, relay bool, caps byte, extra int, log *slog.Logger) *Bind {
	return &Bind{
		priv: priv, pub: PublicKey(priv), relay: relay, caps: caps, extra: extra, log: log,
		peers: map[Key]*peerState{}, byAddr: map[netip.AddrPort]Key{}, byMesh: map[netip.Addr]Key{},
		relayKeys: map[Key]bool{}, links: map[Key]*Link{}, observed: map[pingKey]time.Time{},
	}
}

// confirmedFrom 判断 from 是不是最近确认过的 k 的地址（QUIC 接受连接时核对来源）。
func (b *Bind) confirmedFrom(k Key, from netip.AddrPort) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[k]
	if p == nil {
		return false
	}
	t, ok := p.confirmed[from]
	return ok && time.Since(t) < directTimeout
}

// sock 返回第 i 个本地端口，没有时用主端口。调用时持有 b.mu。
func (b *Bind) sock(i int) *net.UDPConn {
	if i >= 0 && i < len(b.conns) {
		return b.conns[i]
	}
	if len(b.conns) > 0 {
		return b.conns[0]
	}
	return nil
}

// ---- conn.Bind ----

func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.conns) > 0 {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	b.conns = []*net.UDPConn{c}
	for range b.extra {
		x, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			b.log.Warn("mesh 备用端口打不开", "err", err)
			continue
		}
		b.conns = append(b.conns, x)
	}
	b.port = uint16(c.LocalAddr().(*net.UDPAddr).Port)
	b.recv = make(chan delivery, 256)
	b.done = make(chan struct{})
	fns := make([]conn.ReceiveFunc, 0, len(b.conns)+1)
	for i, x := range b.conns {
		fns = append(fns, b.receiveUDP(x, i))
	}
	return append(fns, b.receiveRelayed(b.recv, b.done)), b.port, nil
}

func (b *Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.conns) == 0 {
		return nil
	}
	var errs []error
	for _, c := range b.conns {
		errs = append(errs, c.Close())
	}
	close(b.done)
	b.conns, b.recv, b.done = nil, nil, nil
	return errors.Join(errs...)
}

func (b *Bind) SetMark(uint32) error { return nil }

// BatchSize：一次最多交给 WireGuard 这么多个包。FEC 算回丢掉的包时，要和后面扣着的包按顺序一起交。
func (b *Bind) BatchSize() int { return 32 }

func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	k, err := ParseKey(raw)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.peers[k]; p != nil {
		return p.ep, nil
	}
	return nil, errors.New("未知的对端")
}

func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	e, ok := ep.(*endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	for _, buf := range bufs {
		b.sendTo(e.key, buf)
	}
	return nil
}

// receiveUDP 是第 local 个本地端口的收包循环。
func (b *Bind) receiveUDP(c *net.UDPConn, local int) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, addr, err := c.ReadFromUDPAddrPort(packets[0])
			if err != nil {
				return 0, err
			}
			if n == 0 {
				continue // 空包：下面要读第一个字节，UDP 来源地址又能伪造
			}
			addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
			pkt := packets[0][:n]
			if isControl(pkt) {
				if _, ok := parse(pkt); ok {
					if ep, payload := b.handleControl(c, local, addr, pkt); ep != nil {
						sizes[0] = copy(packets[0], payload)
						eps[0] = ep
						return 1, nil
					}
					continue
				}
				// 不是控制包：碰巧以 "NAGI" 开头的 QUIC 包，往下走。
			}
			b.mu.Lock()
			k, ok := b.byAddr[addr]
			var ep *endpoint
			var dec *fecDecoder
			p := b.peers[k]
			if ok && p != nil {
				ep, dec = p.ep, p.dec
			}
			q := b.quic
			if ep != nil && q != nil && isQUIC(pkt) {
				p.quicRx += uint64(len(pkt))
			}
			b.mu.Unlock()
			if ep == nil {
				continue // 来源地址没确认过，丢弃
			}
			if isQUIC(pkt) {
				if q != nil {
					q.deliver(pkt, addr)
				}
				continue
			}
			if pkt[0] == fecData || pkt[0] == fecParity {
				out, waiting := dec.push(pkt, time.Now())
				if waiting {
					b.holdFEC(k, dec)
				}
				if n := b.fill(out, ep, packets, sizes, eps, k); n > 0 {
					return n, nil
				}
				continue
			}
			sizes[0] = n
			eps[0] = ep
			return 1, nil
		}
	}
}

// fill 把 FEC 交出来的包按顺序放进 WireGuard 的缓冲区，放不下的经中转的通道交。
func (b *Bind) fill(out [][]byte, ep *endpoint, packets [][]byte, sizes []int, eps []conn.Endpoint, k Key) int {
	n := 0
	for _, x := range out {
		if n < len(packets) && len(x) <= len(packets[n]) {
			sizes[n] = copy(packets[n], x)
			eps[n] = ep
			n++
			continue
		}
		b.queue(k, x, true)
	}
	return n
}

// holdFEC 在 fecHold 后放行扣着、还没等到的包。同一个对端同时只排一个。
func (b *Bind) holdFEC(k Key, dec *fecDecoder) {
	if !dec.timer.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(fecHold, func() {
		dec.timer.Store(false)
		out, waiting := dec.release(time.Now())
		for _, x := range out {
			b.queue(k, x, true)
		}
		if waiting {
			b.holdFEC(k, dec)
		}
	})
}

func (b *Bind) receiveRelayed(ch chan delivery, done chan struct{}) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		select {
		case d := <-ch:
			sizes[0] = copy(packets[0], d.data)
			eps[0] = d.ep
			return 1, nil
		case <-done:
			return 0, net.ErrClosed
		}
	}
}

// ---- 发送 ----

func (b *Bind) write(c *net.UDPConn, pkt []byte, to netip.AddrPort) {
	if c == nil || (dropUDP != nil && dropUDP(uint16(c.LocalAddr().(*net.UDPAddr).Port), to)) {
		return
	}
	_, _ = c.WriteToUDPAddrPort(pkt, to)
}

// sendTo 按 直连 → 直接的控制连接 → UDP 中转 → TLS 中转 的顺序选一条路径发送。
func (b *Bind) sendTo(dst Key, pkt []byte) {
	now := time.Now()
	b.mu.Lock()
	p := b.peers[dst]
	if p == nil {
		b.mu.Unlock()
		return
	}
	if p.directAlive(now) {
		addr, c := p.direct, b.sock(p.directConn)
		var enc *fecEncoder
		if b.caps&capFEC != 0 && p.has(capFEC, now) {
			enc = p.enc
		}
		b.mu.Unlock()
		if enc == nil {
			b.write(c, pkt, addr)
			return
		}
		data, parity := enc.frame(pkt, now, func() { b.flushFEC(dst) })
		b.write(c, data, addr)
		for _, x := range parity {
			b.write(c, x, addr)
		}
		return
	}
	if link := b.links[dst]; link != nil {
		b.mu.Unlock()
		_ = (*link)(b.pub, dst, pkt)
		return
	}
	// 经和两端都直连的节点中转（面板挑的，延迟低的在前），比绕到面板近。
	if _, rp, ok := b.pickNodeRelay(p, now, true); ok {
		addr, c := rp.direct, b.sock(rp.directConn)
		b.mu.Unlock()
		b.write(c, encodeRelay(typeRelaySend, dst, pkt), addr)
		return
	}
	for _, r := range p.relays {
		if rp := b.peers[r]; rp != nil && rp.directAlive(now) {
			addr, c := rp.direct, b.sock(rp.directConn)
			b.mu.Unlock()
			b.write(c, encodeRelay(typeRelaySend, dst, pkt), addr)
			return
		}
	}
	for _, r := range p.relays {
		if link := b.links[r]; link != nil {
			b.mu.Unlock()
			_ = (*link)(b.pub, dst, pkt)
			return
		}
	}
	if len(pkt) > 0 && pkt[0] == msgInitiation {
		p.pendingInit, p.pendingInitAt = append([]byte(nil), pkt...), now
	}
	b.mu.Unlock()
}

// flushFEC 在一组没凑满、等了 fecFlush 以后发出这一组的校验包。
func (b *Bind) flushFEC(dst Key) {
	now := time.Now()
	b.mu.Lock()
	p := b.peers[dst]
	if p == nil || !p.directAlive(now) {
		b.mu.Unlock()
		return
	}
	addr, enc, c := p.direct, p.enc, b.sock(p.directConn)
	b.mu.Unlock()
	for _, x := range enc.flush(now) {
		b.write(c, x, addr)
	}
}

// flushPending 补发暂存的握手包。有新路径可用时调用；仍然没有路径的会被 sendTo 重新暂存。
func (b *Bind) flushPending() {
	type pending struct {
		key Key
		pkt []byte
	}
	var out []pending
	now := time.Now()
	b.mu.Lock()
	for k, p := range b.peers {
		if p.pendingInit != nil {
			if now.Sub(p.pendingInitAt) < pendingInitTTL {
				out = append(out, pending{k, p.pendingInit})
			}
			p.pendingInit = nil
		}
	}
	b.mu.Unlock()
	for _, x := range out {
		b.sendTo(x.key, x.pkt)
	}
}

// forward 替 src 把包转给 dst（面板）。
func (b *Bind) forward(src, dst Key, payload []byte) {
	b.relayRx.Add(uint64(len(payload)))
	now := time.Now()
	b.mu.Lock()
	p := b.peers[dst]
	var addr netip.AddrPort
	var c *net.UDPConn
	var link *Link
	if p != nil && p.directAlive(now) {
		addr, c = p.direct, b.sock(p.directConn)
	} else {
		link = b.links[dst]
	}
	b.mu.Unlock()
	switch {
	case addr.IsValid():
		b.write(c, encodeRelay(typeRelayDeliver, src, payload), addr)
	case link != nil:
		_ = (*link)(src, dst, payload)
	default:
		return
	}
	b.relayTx.Add(uint64(len(payload)))
}

// forwardNode 是节点替 src 把包转给 dst：只在和 dst 直连时经 UDP 转发（不经控制连接、不再转给别人），
// 来源超过配额的丢掉。src 已经由调用方按直连地址确认过。
func (b *Bind) forwardNode(src, dst Key, payload []byte, now time.Time) {
	b.mu.Lock()
	p := b.peers[dst]
	if p == nil || dst == src || !p.directAlive(now) || !b.relayAllowed(src, len(payload), now) {
		b.mu.Unlock()
		return
	}
	addr, c := p.direct, b.sock(p.directConn)
	b.mu.Unlock()
	b.relayRx.Add(uint64(len(payload)))
	b.write(c, encodeRelay(typeRelayDeliver, src, payload), addr)
	b.relayTx.Add(uint64(len(payload)))
}

// deliver 把经中转收到的包交给 WireGuard。
func (b *Bind) deliver(from Key, data []byte) { b.queue(from, data, false) }

// queue 把包放进交给 WireGuard 的队列。wait 为 false 时队列满了就丢（中转的包，WireGuard 会重传）；
// 为 true 时排队等（FEC 扣下又放行的包，一次可能有上千个，丢了 TCP 会一下子降到底）。
func (b *Bind) queue(from Key, data []byte, wait bool) {
	b.mu.Lock()
	ch, done := b.recv, b.done
	var ep *endpoint
	if p := b.peers[from]; p != nil {
		ep = p.ep
	}
	b.mu.Unlock()
	if ch == nil || ep == nil {
		return
	}
	d := delivery{ep: ep, data: append([]byte(nil), data...)}
	if wait {
		select {
		case ch <- d:
		case <-done:
		}
		return
	}
	select {
	case ch <- d:
	case <-done:
	default:
	}
}

// HandleLink 处理经控制连接收到的包。
func (b *Bind) HandleLink(src, dst Key, payload []byte) {
	if dst == b.pub {
		b.deliver(src, payload)
		return
	}
	if b.relay {
		b.forward(src, dst, payload)
	}
}

// AttachLink 登记到某个对端的控制连接，返回取消登记的函数。
// 重连时新连接可能先登记，旧连接后取消，所以取消时只删除自己登记的那一个。
func (b *Bind) AttachLink(k Key, l Link) (detach func()) {
	entry := &l
	b.mu.Lock()
	b.links[k] = entry
	b.mu.Unlock()
	b.flushPending()
	return func() {
		b.mu.Lock()
		if b.links[k] == entry {
			delete(b.links, k)
		}
		b.mu.Unlock()
	}
}

// ---- 控制包 ----

// newPing 生成从第 local 个本地端口发往 addr 的 ping。调用时持有 b.mu。
func (b *Bind) newPing(p *peerState, local int, addr netip.AddrPort, now time.Time) []byte {
	var txid [txidLen]byte
	_, _ = rand.Read(txid[:])
	p.pings[txid] = pingInfo{addr: addr, local: local, sent: now}
	p.lastPing[pingKey{local, addr}] = now
	return encodePing(b.pub, txid, p.mac)
}

// handleControl 处理第 local 个本地端口 c 收到的控制包，回复也从 c 发出。
// 返回非空 endpoint 时，payload 是经中转送达的 WireGuard 包。
func (b *Bind) handleControl(c *net.UDPConn, local int, from netip.AddrPort, pkt []byte) (*endpoint, []byte) {
	m, ok := parse(pkt)
	if !ok {
		return nil, nil
	}
	now := time.Now()
	switch m.typ {
	case typePing:
		b.mu.Lock()
		p := b.peers[m.key]
		if p == nil || !verify(pkt, p.mac) {
			b.mu.Unlock()
			return nil, nil
		}
		p.learned[from] = now
		pong := encodePong(b.pub, m.txid, from, p.mac)
		// 对方能找到我们，我们也立刻反向探测，加快打洞。刚发过的 ping 可能在对方还没加上我们时就到了、被丢掉，
		// 所以只做很短的限流（ping 带 MAC，只有成员触发得了）。
		var ping []byte
		if !(p.direct == from && p.directAlive(now)) && now.Sub(p.lastPing[pingKey{local, from}]) > 100*time.Millisecond {
			ping = b.newPing(p, local, from, now)
		}
		b.mu.Unlock()
		b.write(c, pong, from)
		if ping != nil {
			b.write(c, ping, from)
		}
	case typePong:
		b.mu.Lock()
		p := b.peers[m.key]
		if p == nil || !verify(pkt, p.mac) {
			b.mu.Unlock()
			return nil, nil
		}
		info, ok := p.pings[m.txid]
		if !ok || info.addr != from || info.local != local {
			b.mu.Unlock()
			return nil, nil
		}
		delete(p.pings, m.txid)
		if m.observed.IsValid() {
			b.observed[pingKey{local, m.observed}] = now
		}
		// 这个地址确认能通：它发来的包都收（对方可能从这里发，见 confirmed）。
		p.confirmed[from] = now
		b.byAddr[from] = p.key
		alive := p.directAlive(now)
		// 直连可用、最近还确认过，经别的地址或本地端口也通了（反向探测的回复、对方的备用端口）：
		// 发还走现在这个，不来回换。现在这个有一阵没回了（对方换了地址）才换过去，不用等它超时。
		if alive && (p.direct != from || p.directConn != local) && now.Sub(p.directAt) < pingDirect+2*time.Second {
			b.mu.Unlock()
			return nil, nil
		}
		newPath := !alive
		if p.direct != from || p.directConn != local {
			b.log.Debug("mesh 直连可用", "peer", p.key.Short(), "addr", from, "local_port", c.LocalAddr().(*net.UDPAddr).Port)
			newPath = true
		}
		p.direct, p.directConn, p.directAt, p.rtt = from, local, now, now.Sub(info.sent)
		b.mu.Unlock()
		// 到这个对端（以及经它中转的对端）有了路径：补发之前发不出去的握手，告诉对方自己的能力。
		if newPath {
			b.flushPending()
			b.mu.Lock()
			caps := b.capsPacket(p)
			b.mu.Unlock()
			b.write(c, caps, from)
		}
	case typeCaps:
		b.mu.Lock()
		p := b.peers[m.key]
		if p == nil || !verify(pkt, p.mac) {
			b.mu.Unlock()
			return nil, nil
		}
		newQUIC := m.caps&capQUIC != 0 && !p.has(capQUIC, now)
		p.caps, p.capsAt = m.caps, now
		switch m.lossPPM {
		case lossUnknown:
		case lossUseless:
			p.enc.giveUp(now)
		default:
			p.lossPPM, p.lossAt = m.lossPPM, now
			p.enc.adjust(m.lossPPM, now)
		}
		onCaps := b.onCaps
		b.mu.Unlock()
		if newQUIC && onCaps != nil {
			go onCaps(m.key)
		}
	case typeRelaySend:
		if !b.relay && !b.nodeRelay.Load() {
			return nil, nil
		}
		b.mu.Lock()
		src, ok := b.byAddr[from]
		alive := ok && b.peers[src] != nil && b.peers[src].directAlive(now)
		b.mu.Unlock()
		if !alive {
			return nil, nil
		}
		if m.key == b.pub {
			b.mu.Lock()
			ep := b.peers[src].ep
			b.mu.Unlock()
			return ep, m.payload
		}
		if b.relay {
			b.forward(src, m.key, m.payload)
		} else {
			b.forwardNode(src, m.key, m.payload, now)
		}
	case typeRelayDeliver:
		b.mu.Lock()
		defer b.mu.Unlock()
		relay, ok := b.byAddr[from]
		p := b.peers[m.key]
		if !ok || !b.relayKeys[relay] || p == nil {
			return nil, nil
		}
		b.relayHeard(p, relay, now)
		return p.ep, m.payload
	}
	return nil, nil
}

// ---- 维护 ----

// setPeers 更新对端列表。
func (b *Bind) setPeers(peers []Peer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := map[Key]bool{}
	b.relayKeys = map[Key]bool{}
	b.byMesh = map[netip.Addr]Key{}
	for _, pe := range peers {
		if pe.Key == b.pub {
			continue
		}
		p := b.peers[pe.Key]
		if p == nil {
			mac, ok := macKey(b.priv, pe.Key)
			if !ok {
				continue // SetPeers 已经滤掉了，这里再防一次
			}
			p = &peerState{
				key: pe.Key, ep: &endpoint{key: pe.Key}, mac: mac,
				learned: map[netip.AddrPort]time.Time{}, lastPing: map[pingKey]time.Time{},
				pings: map[[txidLen]byte]pingInfo{}, confirmed: map[netip.AddrPort]time.Time{},
				// 一开始就加 1 个校验包，对方报告丢包很少时再关：等测出丢包再开，TCP 早就降速了。
				enc: newFECEncoder(1), dec: newFECDecoder(),
			}
			b.peers[pe.Key] = p
		}
		keep[pe.Key] = true
		p.candidates = pe.Endpoints
		if len(p.candidates) > maxCandidates {
			p.candidates = p.candidates[:maxCandidates]
		}
		p.relays = pe.Relays
		p.nodeRelays = nil
		for _, r := range pe.NodeRelays {
			if r != b.pub && r != pe.Key {
				p.nodeRelays = append(p.nodeRelays, r)
			}
		}
		// 中转方转来的包由 WireGuard 按原发送方验证，中转方伪造不了，所以只核对它是不是中转方。
		for _, r := range append(append([]Key(nil), pe.Relays...), p.nodeRelays...) {
			b.relayKeys[r] = true
		}
		b.byMesh[Address(pe.Key)] = pe.Key
	}
	for k, p := range b.peers {
		if !keep[k] {
			for a := range p.confirmed {
				if b.byAddr[a] == k {
					delete(b.byAddr, a)
				}
			}
			delete(b.peers, k)
		}
	}
}

// maintain 定期探测：直连可用时保活，不可用时探测所有候选地址。
func (b *Bind) maintain(now time.Time) {
	type out struct {
		c    *net.UDPConn
		pkt  []byte
		addr netip.AddrPort
	}
	var sends []out
	b.mu.Lock()
	for _, p := range b.peers {
		for a, t := range p.learned {
			if now.Sub(t) > forget {
				delete(p.learned, a)
			}
		}
		for a, t := range p.lastPing {
			if now.Sub(t) > forget {
				delete(p.lastPing, a)
			}
		}
		for id, info := range p.pings {
			if now.Sub(info.sent) > 30*time.Second {
				delete(p.pings, id)
			}
		}
		for a, t := range p.confirmed {
			if now.Sub(t) > directTimeout && !(a == p.direct && p.directAlive(now)) {
				delete(p.confirmed, a)
				if b.byAddr[a] == p.key {
					delete(b.byAddr, a)
				}
			}
		}
		if p.directAlive(now) {
			c := b.sock(p.directConn)
			if now.Sub(p.lastPing[pingKey{p.directConn, p.direct}]) >= pingDirect {
				sends = append(sends, out{c, b.newPing(p, p.directConn, p.direct, now), p.direct})
			}
			// 能力包每轮都发（2 秒一次）：丢包率变了，对方能很快调整冗余。
			sends = append(sends, out{c, b.capsPacket(p), p.direct})
			continue
		}
		if p.direct.IsValid() {
			b.log.Debug("mesh 直连断开", "peer", p.key.Short(), "addr", p.direct)
			delete(p.confirmed, p.direct)
			if b.byAddr[p.direct] == p.key {
				delete(b.byAddr, p.direct)
			}
			p.direct, p.directConn = netip.AddrPort{}, 0
		}
		addrs := append([]netip.AddrPort(nil), p.candidates...)
		for a := range p.learned {
			if !slices.Contains(addrs, a) {
				addrs = append(addrs, a)
			}
		}
		// 每个地址从每个本地端口都探测：哪个端口先收到回复就用哪个。
		for _, a := range addrs {
			for i, c := range b.conns {
				if now.Sub(p.lastPing[pingKey{i, a}]) >= pingSearch {
					sends = append(sends, out{c, b.newPing(p, i, a, now), a})
				}
			}
		}
	}
	for a, t := range b.observed {
		if now.Sub(t) > forget {
			delete(b.observed, a)
		}
	}
	b.mu.Unlock()
	for _, s := range sends {
		b.write(s.c, s.pkt, s.addr)
	}
}

// announce 马上给所有直连的成员发能力包（能力变了时），不等下一轮 maintain。
func (b *Bind) announce() {
	now := time.Now()
	type out struct {
		c    *net.UDPConn
		pkt  []byte
		addr netip.AddrPort
	}
	var sends []out
	b.mu.Lock()
	for _, p := range b.peers {
		if p.directAlive(now) {
			sends = append(sends, out{b.sock(p.directConn), b.capsPacket(p), p.direct})
		}
	}
	b.mu.Unlock()
	for _, s := range sends {
		b.write(s.c, s.pkt, s.addr)
	}
}

// lossFresh：丢包率这么久没有更新（没什么流量）就不再显示。
const lossFresh = 5 * time.Minute

// capsPacket 是发给 p 的能力包，带上收到它的包的丢包率。调用时持有 b.mu。
func (b *Bind) capsPacket(p *peerState) []byte {
	loss := uint32(lossUnknown)
	ppm, ok, useless := p.dec.lossPPM()
	if ok {
		p.rxLoss, p.rxLossAt = ppm, time.Now()
	}
	if useless {
		loss = lossUseless
	} else if ok {
		loss = ppm
	}
	return encodeCaps(b.pub, b.caps, loss, p.mac)
}

// PeerStatus 是到某个对端的当前路径。
type PeerStatus struct {
	Key Key
	// direct、link（直接经控制连接）、udp-relay、tls-relay 或 none。
	Path   string
	Direct netip.AddrPort
	// LocalPort 是直连用的本地端口（主端口或备用端口）。
	LocalPort uint16
	Via    Key // 中转的面板
	RTT    time.Duration
	// FEC 是发给对方时每 10 个包加的校验包个数，-1 表示没用 FEC（对方不支持或不是直连）。
	FEC int
	// LossPPM 是对方报告的丢包率（百万分之几），Recovered 是靠 FEC 恢复的、来自对方的包。
	LossPPM   uint32
	Recovered uint64
	// TxLoss、RxLoss 是最近测到的两个方向的丢包率（百万分之几）：TxLoss 是发给对方的包丢了多少
	// （对方的 FEC 报告或 QUIC 的统计，取新的），RxLoss 是收对方的包丢了多少（FEC 序号）。没测到时为 -1。
	TxLoss, RxLoss int64
	txLossAt       time.Time
	// QUIC：和对方之间有建好的 QUIC 连接，mesh 上的 TCP 连接走它（见 quic.go）。
	QUIC bool
	// 没有直连时用来说明原因：Tried 是正在探测的对方地址数（对方上报的加上从它的探测学到的），
	// Heard 表示最近收到过对方发来的探测（对方的包能到这一端）。
	Tried int
	Heard bool
	// quicBytes：收、发的 QUIC 字节数，Status 把它加进 RxBytes、TxBytes。
	quicBytes [2]uint64
	// fecBytes：FEC 在收、发两个方向上比 WireGuard 统计的多出的字节数，Status 同样加进去。
	fecBytes [2]int64
}

func (b *Bind) status() []PeerStatus {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []PeerStatus
	for _, p := range b.peers {
		s := PeerStatus{Key: p.key, Path: "none", FEC: -1, LossPPM: p.lossPPM, Recovered: p.dec.recoveredCount(), quicBytes: [2]uint64{p.quicRx, p.quicTx},
			fecBytes: [2]int64{p.dec.overheadBytes(), int64(p.enc.overheadBytes())},
			TxLoss:   -1, RxLoss: -1}
		if p.directAlive(now) {
			if now.Sub(p.lossAt) < lossFresh {
				s.TxLoss, s.txLossAt = int64(p.lossPPM), p.lossAt
			}
			if now.Sub(p.rxLossAt) < lossFresh {
				s.RxLoss = int64(p.rxLoss)
			}
		}
		if !p.directAlive(now) {
			s.Heard = len(p.learned) > 0
			s.Tried = len(p.candidates)
			for a := range p.learned {
				if !slices.Contains(p.candidates, a) {
					s.Tried++
				}
			}
		}
		switch {
		case p.directAlive(now):
			s.Path, s.Direct, s.RTT = "direct", p.direct, p.rtt
			if c := b.sock(p.directConn); c != nil {
				s.LocalPort = uint16(c.LocalAddr().(*net.UDPAddr).Port)
			}
			if b.caps&capFEC != 0 && p.has(capFEC, now) {
				s.FEC = p.enc.parityCount()
			}
		case b.links[p.key] != nil:
			s.Path = "link"
		default:
			if r, _, ok := b.pickNodeRelay(p, now, false); ok {
				s.Path, s.Via = "node-relay", r
			}
			if s.Path == "none" {
				for _, r := range p.relays {
					if rp := b.peers[r]; rp != nil && rp.directAlive(now) {
						s.Path, s.Via = "udp-relay", r
						break
					}
				}
			}
			if s.Path == "none" {
				for _, r := range p.relays {
					if b.links[r] != nil {
						s.Path, s.Via = "tls-relay", r
						break
					}
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// observedAddrs 返回别人看到的我们主端口的地址（经过 NAT 以后的公网地址）：上报给面板，别人往这里连。
func (b *Bind) observedAddrs() []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []netip.AddrPort
	for k := range b.observed {
		if k.local == 0 {
			out = append(out, k.addr)
		}
	}
	return out
}

// observedBySocket 返回每个本地端口被别人看到的地址，用来判断 NAT（见 natKind）。
func (b *Bind) observedBySocket() map[int][]netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[int][]netip.AddrPort{}
	for k := range b.observed {
		out[k.local] = append(out[k.local], k.addr)
	}
	return out
}
