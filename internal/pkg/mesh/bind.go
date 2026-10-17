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
)

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
	addr netip.AddrPort
	sent time.Time
}

type peerState struct {
	key        Key
	ep         *endpoint
	mac        [32]byte
	candidates []netip.AddrPort             // 别人告诉我们的地址
	learned    map[netip.AddrPort]time.Time // 从对方的 ping 学到的地址
	lastPing   map[netip.AddrPort]time.Time // 每个地址上次发 ping 的时间
	pings      map[[txidLen]byte]pingInfo   // 等待 pong 的 ping
	direct     netip.AddrPort               // 确认能直连的地址
	directAt   time.Time                    // 最近一次 pong 的时间
	rtt        time.Duration
	relays     []Key // 可以代为中转的面板
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
	log       *slog.Logger

	mu        sync.Mutex
	conn      *net.UDPConn
	port      uint16
	peers     map[Key]*peerState
	byAddr    map[netip.AddrPort]Key // 已确认的直连地址 → 对端
	byMesh    map[netip.Addr]Key     // mesh 地址 → 对端
	relayKeys map[Key]bool
	links     map[Key]*Link
	observed  map[netip.AddrPort]time.Time // 别人看到的我们的地址
	recv      chan delivery
	done      chan struct{}
}

func newBind(priv Key, relay bool, log *slog.Logger) *Bind {
	return &Bind{
		priv: priv, pub: PublicKey(priv), relay: relay, log: log,
		peers: map[Key]*peerState{}, byAddr: map[netip.AddrPort]Key{}, byMesh: map[netip.Addr]Key{},
		relayKeys: map[Key]bool{}, links: map[Key]*Link{}, observed: map[netip.AddrPort]time.Time{},
	}
}

// ---- conn.Bind ----

func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	b.conn = c
	b.port = uint16(c.LocalAddr().(*net.UDPAddr).Port)
	b.recv = make(chan delivery, 256)
	b.done = make(chan struct{})
	return []conn.ReceiveFunc{b.receiveUDP(c), b.receiveRelayed(b.recv, b.done)}, b.port, nil
}

func (b *Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	close(b.done)
	b.conn, b.recv, b.done = nil, nil, nil
	return err
}

func (b *Bind) SetMark(uint32) error { return nil }
func (b *Bind) BatchSize() int       { return 1 }

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

func (b *Bind) receiveUDP(c *net.UDPConn) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, addr, err := c.ReadFromUDPAddrPort(packets[0])
			if err != nil {
				return 0, err
			}
			addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
			pkt := packets[0][:n]
			if isControl(pkt) {
				if ep, payload := b.handleControl(c, addr, pkt); ep != nil {
					sizes[0] = copy(packets[0], payload)
					eps[0] = ep
					return 1, nil
				}
				continue
			}
			b.mu.Lock()
			k, ok := b.byAddr[addr]
			var ep *endpoint
			if ok && b.peers[k] != nil {
				ep = b.peers[k].ep
			}
			b.mu.Unlock()
			if ep == nil {
				continue // 来源地址没确认过，丢弃
			}
			sizes[0] = n
			eps[0] = ep
			return 1, nil
		}
	}
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
	if c == nil || (dropUDP != nil && dropUDP(b.port, to)) {
		return
	}
	_, _ = c.WriteToUDPAddrPort(pkt, to)
}

// sendTo 按 直连 → 直接的控制连接 → UDP 中转 → TLS 中转 的顺序选一条路径发送。
func (b *Bind) sendTo(dst Key, pkt []byte) {
	now := time.Now()
	b.mu.Lock()
	p := b.peers[dst]
	c := b.conn
	if p == nil {
		b.mu.Unlock()
		return
	}
	if p.directAlive(now) {
		addr := p.direct
		b.mu.Unlock()
		b.write(c, pkt, addr)
		return
	}
	if link := b.links[dst]; link != nil {
		b.mu.Unlock()
		_ = (*link)(b.pub, dst, pkt)
		return
	}
	for _, r := range p.relays {
		if rp := b.peers[r]; rp != nil && rp.directAlive(now) {
			addr := rp.direct
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
	b.mu.Unlock()
}

// forward 替 src 把包转给 dst（面板）。
func (b *Bind) forward(src, dst Key, payload []byte) {
	now := time.Now()
	b.mu.Lock()
	p := b.peers[dst]
	c := b.conn
	var addr netip.AddrPort
	var link *Link
	if p != nil && p.directAlive(now) {
		addr = p.direct
	} else {
		link = b.links[dst]
	}
	b.mu.Unlock()
	switch {
	case addr.IsValid():
		b.write(c, encodeRelay(typeRelayDeliver, src, payload), addr)
	case link != nil:
		_ = (*link)(src, dst, payload)
	}
}

// deliver 把经中转收到的包交给 WireGuard。
func (b *Bind) deliver(from Key, data []byte) {
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
	select {
	case ch <- delivery{ep: ep, data: append([]byte(nil), data...)}:
	case <-done:
	default: // 队列满了就丢，WireGuard 会重传
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
	return func() {
		b.mu.Lock()
		if b.links[k] == entry {
			delete(b.links, k)
		}
		b.mu.Unlock()
	}
}

// ---- 控制包 ----

func (b *Bind) newPing(p *peerState, addr netip.AddrPort, now time.Time) []byte {
	var txid [txidLen]byte
	_, _ = rand.Read(txid[:])
	p.pings[txid] = pingInfo{addr: addr, sent: now}
	p.lastPing[addr] = now
	return encodePing(b.pub, txid, p.mac)
}

// handleControl 处理控制包。返回非空 endpoint 时，payload 是经中转送达的 WireGuard 包。
func (b *Bind) handleControl(c *net.UDPConn, from netip.AddrPort, pkt []byte) (*endpoint, []byte) {
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
		// 对方能找到我们，我们也立刻反向探测，加快打洞。
		var ping []byte
		if !(p.direct == from && p.directAlive(now)) && now.Sub(p.lastPing[from]) > time.Second {
			ping = b.newPing(p, from, now)
		}
		b.mu.Unlock()
		b.write(c, pong, from)
		if ping != nil {
			b.write(c, ping, from)
		}
	case typePong:
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.peers[m.key]
		if p == nil || !verify(pkt, p.mac) {
			return nil, nil
		}
		info, ok := p.pings[m.txid]
		if !ok || info.addr != from {
			return nil, nil
		}
		delete(p.pings, m.txid)
		if p.direct != from {
			if p.direct.IsValid() {
				delete(b.byAddr, p.direct)
			}
			b.log.Debug("mesh 直连可用", "peer", p.key.Short(), "addr", from)
		}
		p.direct, p.directAt, p.rtt = from, now, now.Sub(info.sent)
		b.byAddr[from] = p.key
		if m.observed.IsValid() {
			b.observed[m.observed] = now
		}
	case typeRelaySend:
		if !b.relay {
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
		b.forward(src, m.key, m.payload)
	case typeRelayDeliver:
		b.mu.Lock()
		defer b.mu.Unlock()
		relay, ok := b.byAddr[from]
		p := b.peers[m.key]
		if !ok || !b.relayKeys[relay] || p == nil {
			return nil, nil
		}
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
		keep[pe.Key] = true
		p := b.peers[pe.Key]
		if p == nil {
			p = &peerState{
				key: pe.Key, ep: &endpoint{key: pe.Key}, mac: macKey(b.priv, pe.Key),
				learned: map[netip.AddrPort]time.Time{}, lastPing: map[netip.AddrPort]time.Time{},
				pings: map[[txidLen]byte]pingInfo{},
			}
			b.peers[pe.Key] = p
		}
		p.candidates = pe.Endpoints
		if len(p.candidates) > maxCandidates {
			p.candidates = p.candidates[:maxCandidates]
		}
		p.relays = pe.Relays
		for _, r := range pe.Relays {
			b.relayKeys[r] = true
		}
		b.byMesh[Address(pe.Key)] = pe.Key
	}
	for k, p := range b.peers {
		if !keep[k] {
			if p.direct.IsValid() {
				delete(b.byAddr, p.direct)
			}
			delete(b.peers, k)
		}
	}
}

// maintain 定期探测：直连可用时保活，不可用时探测所有候选地址。
func (b *Bind) maintain(now time.Time) {
	type out struct {
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
		if p.directAlive(now) {
			if now.Sub(p.lastPing[p.direct]) >= pingDirect {
				sends = append(sends, out{b.newPing(p, p.direct, now), p.direct})
			}
			continue
		}
		if p.direct.IsValid() {
			b.log.Debug("mesh 直连断开", "peer", p.key.Short(), "addr", p.direct)
			delete(b.byAddr, p.direct)
			p.direct = netip.AddrPort{}
		}
		addrs := append([]netip.AddrPort(nil), p.candidates...)
		for a := range p.learned {
			if !slices.Contains(addrs, a) {
				addrs = append(addrs, a)
			}
		}
		for _, a := range addrs {
			if now.Sub(p.lastPing[a]) >= pingSearch {
				sends = append(sends, out{b.newPing(p, a, now), a})
			}
		}
	}
	for a, t := range b.observed {
		if now.Sub(t) > forget {
			delete(b.observed, a)
		}
	}
	c := b.conn
	b.mu.Unlock()
	for _, s := range sends {
		b.write(c, s.pkt, s.addr)
	}
}

// PeerStatus 是到某个对端的当前路径。
type PeerStatus struct {
	Key Key
	// direct、link（直接经控制连接）、udp-relay、tls-relay 或 none。
	Path   string
	Direct netip.AddrPort
	Via    Key // 中转的面板
	RTT    time.Duration
}

func (b *Bind) status() []PeerStatus {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []PeerStatus
	for _, p := range b.peers {
		s := PeerStatus{Key: p.key, Path: "none"}
		switch {
		case p.directAlive(now):
			s.Path, s.Direct, s.RTT = "direct", p.direct, p.rtt
		case b.links[p.key] != nil:
			s.Path = "link"
		default:
			for _, r := range p.relays {
				if rp := b.peers[r]; rp != nil && rp.directAlive(now) {
					s.Path, s.Via = "udp-relay", r
					break
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

// observedAddrs 返回别人看到的我们的地址（经过 NAT 以后的公网地址）。
func (b *Bind) observedAddrs() []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []netip.AddrPort
	for a := range b.observed {
		out = append(out, a)
	}
	return out
}
