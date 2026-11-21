package mesh

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net/netip"
	"time"

	"github.com/apernet/quic-go"
)

// QUIC 最小包长为 1200，不能沿用 1180 的观察包长。实验协议固定上限，不让 PMTU 按直连扩包后堵死中转。
// 最坏的 IPv6 UDP 包：1200 + 37（实验头）+ 16（MAC）+ 37（中转头）+ 48 = 1338 字节。
const routedQUICMTU = 1200

var ErrQUICRoute = errors.New("mesh 实验 QUIC 路径未开启、未授权或逻辑连接尚未就绪")

func routedQUICConfig() *quic.Config {
	cfg := quicConfig()
	cfg.InitialPacketSize = routedQUICMTU
	cfg.DisablePathMTUDiscovery = true
	return cfg
}

func quicLogicalAddr(k Key) netip.AddrPort { return netip.AddrPortFrom(Address(k), 1) }

// routedTransport 是只服务一个成员的实验 QUIC Transport。quic-go 按连接 ID 分流、服务端会跟着新来源迁移地址，
// 共用一个 Transport 时，授权中转 R 能把截到的 A→B 密文改用自己的 R/B MAC 重新封装，B 的连接就迁到 R 的逻辑地址
// （实测会中断原流，撤权后也不能手动恢复）。每个成员单独的 Transport 只收这个成员 MAC 校验过的包，
// 来源地址恒为它的逻辑地址，别人的密文找不到对应的连接 ID，也就不会触发迁移。
type routedTransport struct {
	pc *quicPacketConn
	tr *quic.Transport
	ln *quic.Listener
}

func (rt *routedTransport) close() {
	_ = rt.ln.Close()
	_ = rt.pc.Close() // 先关包连接，Transport.Close 等收包循环退出。
	_ = rt.tr.Close()
}

// routedFor 返回到成员 k 的实验 Transport，没有时现建。调用时不能持有 b.mu（锁顺序和 SetQUICRoute 一样先 q.mu 后 b.mu）。
// 只给当前成员建，数量不超过成员数；成员移除时由 forget 关闭（setPeers 先于 forget，这里查不到就不会再建）。
func (q *quicNet) routedFor(k Key) *routedTransport {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.routing || q.closed || q.routed == nil {
		return nil
	}
	if rt := q.routed[k]; rt != nil {
		return rt
	}
	b := q.m.bind
	b.mu.Lock()
	member := b.peers[k] != nil
	b.mu.Unlock()
	if !member {
		return nil
	}
	pc := &quicPacketConn{b: q.m.bind, routed: true, peer: k, in: make(chan quicPacket, 1024), done: make(chan struct{})}
	reset := quic.StatelessResetKey(sha256.Sum256(append(q.routedSeed[:], k[:]...)))
	tr := &quic.Transport{Conn: pc, StatelessResetKey: &reset}
	ln, err := tr.Listen(q.tls, routedQUICConfig())
	if err != nil {
		_ = pc.Close()
		_ = tr.Close()
		return nil
	}
	rt := &routedTransport{pc: pc, tr: tr, ln: ln}
	q.routed[k] = rt
	go q.accept(ln)
	return rt
}

// routedConn 给 Bind 投递收到的实验包用。
func (q *quicNet) routedConn(k Key) *quicPacketConn {
	if rt := q.routedFor(k); rt != nil {
		return rt.pc
	}
	return nil
}

// SetQUICRoute 手动改变已建立的实验逻辑连接的下一跳；via 为零恢复直连。
// 两端必须事先显式启用 ExperimentalQUICRouting，指定 R 还要求 R 显式启用原型、有独立许可且加入了中转池。
// 不改变 WireGuard 路径，不迁移旧协议连接，不自动选择、检查健康或回退。业务调用方当前没有接入此入口。
func (m *Mesh) SetQUICRoute(peer, via Key) error {
	q := m.quic
	if q == nil || !q.routing || peer == m.self || via == peer || via == m.self {
		return ErrQUICRoute
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.conns[peer] == nil || q.dialing[peer] != nil {
		return ErrQUICRoute
	}
	for c, k := range q.all {
		if k == peer && c.RemoteAddr().String() != quicLogicalAddr(peer).String() {
			return ErrQUICRoute // 连包括同时建立的旧连接在内都不迁移，也不关闭它来伪装成功。
		}
	}
	b := m.bind
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[peer]
	if p == nil || b.caps&capQUIC == 0 {
		return ErrQUICRoute
	}
	if via != (Key{}) {
		r := b.peers[via]
		// 性能路线只经管理员加入中转池的节点；故障兜底的候选排名不算同意。
		if !r.poolRelay() || !r.directAlive(time.Now()) || !r.has(capQUICRoute, time.Now()) {
			return ErrQUICRoute
		}
	}
	p.quicVia = via
	return nil
}

// quicRouteMAC 还绑定不在包内重复传输的终点和实际中转，防止复制到另一条路径或由中转伪造原始发送方。
func quicRouteMAC(mac [32]byte, pkt []byte, target, via Key) []byte {
	h := hmac.New(sha256.New, mac[:])
	h.Write(pkt)
	h.Write(target[:])
	h.Write(via[:])
	return h.Sum(nil)[:macLen]
}

func (c *quicPacketConn) writeRouted(data []byte, addr netip.AddrPort) (int, error) {
	if len(data) > routedQUICMTU {
		return 0, io.ErrShortBuffer
	}
	b := c.b
	now := time.Now()
	b.mu.Lock()
	k, ok := b.byMesh[addr.Addr()]
	p := b.peers[k]
	if !ok || p == nil || k != c.peer || addr != quicLogicalAddr(k) || b.caps&capQUIC == 0 {
		b.mu.Unlock()
		return len(data), nil // 与 UDP 丢包一致，不能因暂时失去下一跳销毁整条 QUIC 连接。
	}
	hop := p
	if p.quicVia != (Key{}) {
		hop = b.peers[p.quicVia]
		if !hop.poolRelay() || !hop.has(capQUICRoute, now) {
			b.mu.Unlock()
			return len(data), nil
		}
	}
	if !hop.directAlive(now) {
		b.mu.Unlock()
		return len(data), nil
	}
	pkt := append(header(typeQUICRoute, b.pub), data...)
	pkt = append(pkt, quicRouteMAC(p.mac, pkt, k, p.quicVia)...)
	if p.quicVia != (Key{}) {
		pkt = encodeRelay(typeRelaySend, k, pkt)
	}
	p.quicTx += uint64(len(pkt))
	conn, to := b.sock(hop.directConn), hop.direct
	b.mu.Unlock()
	b.write(conn, pkt, to)
	return len(data), nil
}

func (b *Bind) handleRoutedQUIC(from netip.AddrPort, relay Key, pkt []byte) {
	msg, ok := parse(pkt)
	if !ok || msg.typ != typeQUICRoute {
		return
	}
	now := time.Now()
	b.mu.Lock()
	p := b.peers[msg.key]
	if b.routedQUIC == nil || b.caps&capQUIC == 0 || p == nil {
		b.mu.Unlock()
		return
	}
	hop := p
	if relay != (Key{}) {
		hop = b.peers[relay]
		if !hop.poolRelay() || !hop.has(capQUICRoute, now) {
			b.mu.Unlock()
			return
		}
	}
	if !hop.directAlive(now) || b.byAddr[from] != hop.key || now.Sub(hop.confirmed[from]) >= directTimeout ||
		!hmac.Equal(pkt[len(pkt)-macLen:], quicRouteMAC(p.mac, pkt[:len(pkt)-macLen], b.pub, relay)) {
		b.mu.Unlock()
		return
	}
	n := len(pkt)
	if relay != (Key{}) {
		n += headerLen
	}
	p.quicRx += uint64(n)
	routed := b.routedQUIC
	b.mu.Unlock()
	// 只投递到这个发送方自己的实验 Transport；仍须 QUIC 解密和原有 TLS exporter/HMAC 身份证明才能交付业务流。
	if q := routed(msg.key); q != nil {
		q.deliver(msg.payload, quicLogicalAddr(msg.key))
	}
}
