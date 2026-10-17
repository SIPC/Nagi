package mesh

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"

	"golang.org/x/crypto/curve25519"
)

// 和 WireGuard 数据包共用一个 UDP 端口的控制包。WireGuard 包的第一个字节是 1～4，后面三个 0，
// 不会和 "NAGI" 开头的控制包混淆。
//
//	ping:          NAGI 1 发送方公钥(32) 事务号(12) MAC(16)
//	pong:          NAGI 2 发送方公钥(32) 事务号(12) 看到的对方地址(18) MAC(16)
//	relay send:    NAGI 3 目标公钥(32) WireGuard 包      节点 → 面板，面板按来源地址认出发送方
//	relay deliver: NAGI 4 来源公钥(32) WireGuard 包      面板 → 节点
//
// ping/pong 用双方 WireGuard 密钥协商出的共享密钥做 MAC，别人伪造不了，也就没法把流量引到别的地址。
// 只有收到对自己 ping 的 pong（事务号对得上）才认为直连可用，所以重放旧包也没用。
var magic = []byte("NAGI")

const (
	typePing         = 1
	typePong         = 2
	typeRelaySend    = 3
	typeRelayDeliver = 4

	headerLen = 4 + 1 + 32
	txidLen   = 12
	macLen    = 16
	pingLen   = headerLen + txidLen + macLen
	pongLen   = headerLen + txidLen + 18 + macLen
)

func isControl(pkt []byte) bool {
	return len(pkt) >= headerLen && bytes.Equal(pkt[:4], magic)
}

// macKey 是和某个对端之间的 MAC 密钥：X25519(自己私钥, 对端公钥)，双方算出来一样。
func macKey(priv, peer Key) [32]byte {
	shared, err := curve25519.X25519(priv[:], peer[:])
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(append([]byte("nagi-mesh-control"), shared...))
}

func sum(key [32]byte, data []byte) []byte {
	m := hmac.New(sha256.New, key[:])
	m.Write(data)
	return m.Sum(nil)[:macLen]
}

func header(typ byte, k Key) []byte {
	b := make([]byte, 0, headerLen)
	b = append(b, magic...)
	b = append(b, typ)
	return append(b, k[:]...)
}

func encodePing(self Key, txid [txidLen]byte, key [32]byte) []byte {
	b := append(header(typePing, self), txid[:]...)
	return append(b, sum(key, b)...)
}

func encodePong(self Key, txid [txidLen]byte, observed netip.AddrPort, key [32]byte) []byte {
	b := append(header(typePong, self), txid[:]...)
	ip := observed.Addr().As16()
	b = append(b, ip[:]...)
	b = binary.BigEndian.AppendUint16(b, observed.Port())
	return append(b, sum(key, b)...)
}

func verify(pkt []byte, key [32]byte) bool {
	n := len(pkt) - macLen
	return n > 0 && hmac.Equal(pkt[n:], sum(key, pkt[:n]))
}

func encodeRelay(typ byte, k Key, payload []byte) []byte {
	return append(header(typ, k), payload...)
}

// parsed 是解析后的控制包。
type parsed struct {
	typ      byte
	key      Key // ping/pong 的发送方、relay send 的目标、relay deliver 的来源
	txid     [txidLen]byte
	observed netip.AddrPort
	payload  []byte
}

func parse(pkt []byte) (parsed, bool) {
	var p parsed
	if !isControl(pkt) {
		return p, false
	}
	p.typ = pkt[4]
	copy(p.key[:], pkt[5:headerLen])
	rest := pkt[headerLen:]
	switch p.typ {
	case typePing:
		if len(pkt) != pingLen {
			return p, false
		}
		copy(p.txid[:], rest)
	case typePong:
		if len(pkt) != pongLen {
			return p, false
		}
		copy(p.txid[:], rest)
		var ip [16]byte
		copy(ip[:], rest[txidLen:])
		port := binary.BigEndian.Uint16(rest[txidLen+16:])
		p.observed = netip.AddrPortFrom(netip.AddrFrom16(ip).Unmap(), port)
	case typeRelaySend, typeRelayDeliver:
		p.payload = rest
	default:
		return p, false
	}
	return p, true
}
