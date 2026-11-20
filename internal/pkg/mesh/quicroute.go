package mesh

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net/netip"
	"slices"
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

// SetQUICRoute 手动改变已建立的实验逻辑连接的下一跳；via 为零恢复直连。
// 两端必须事先显式启用 ExperimentalQUICRouting，指定 R 还要求 R 显式启用原型、独立许可为 true 且在候选内。
// 不改变 WireGuard 路径，不迁移旧协议连接，不自动选择、检查健康或回退。业务调用方当前没有接入此入口。
func (m *Mesh) SetQUICRoute(peer, via Key) error {
	q := m.quic
	if q == nil || q.routedTR == nil || peer == m.self || via == peer || via == m.self {
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
		if r == nil || r.nodeRelayEnabled == nil || !*r.nodeRelayEnabled || !r.directAlive(time.Now()) ||
			!r.has(capQUICRoute, time.Now()) || (!slices.Contains(p.nodeRelays, via) && p.quicVia != via) {
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
	if !ok || p == nil || addr != quicLogicalAddr(k) || b.caps&capQUIC == 0 {
		b.mu.Unlock()
		return len(data), nil // 与 UDP 丢包一致，不能因暂时失去下一跳销毁整条 QUIC 连接。
	}
	hop := p
	if p.quicVia != (Key{}) {
		hop = b.peers[p.quicVia]
		if hop == nil || hop.nodeRelayEnabled == nil || !*hop.nodeRelayEnabled || !hop.has(capQUICRoute, now) {
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
		if hop == nil || hop.nodeRelayEnabled == nil || !*hop.nodeRelayEnabled || !hop.has(capQUICRoute, now) {
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
	q := b.routedQUIC
	b.mu.Unlock()
	// 只投递到实验 Transport；仍须 QUIC 解密和原有 TLS exporter/HMAC 身份证明才能交付业务流。
	q.deliver(msg.payload, quicLogicalAddr(msg.key))
}
