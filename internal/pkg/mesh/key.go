// Package mesh 实现节点和面板之间的 WireGuard mesh：用户态 wireguard-go 加进程内网络栈，
// 不建网卡、不改路由，只有 Nagi 自己的流量（快照、上传、投票）走 mesh。
//
// 每个成员用 WireGuard 公钥标识，mesh 地址由公钥推导。数据包按顺序尝试三条路径：
//  1. UDP 直连（探测包确认能通以后才用）；
//  2. UDP 经面板中转；
//  3. 经面板的控制连接（TLS）中转。
package mesh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/netip"

	"golang.org/x/crypto/curve25519"
)

// Key 是 WireGuard 公钥或私钥。
type Key [32]byte

// ParseKey 从 32 字节解析公钥。
func ParseKey(b []byte) (Key, error) {
	var k Key
	if len(b) != len(k) {
		return k, errors.New("WireGuard 公钥长度不对")
	}
	copy(k[:], b)
	return k, nil
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
func (k Key) hex() string    { return hex.EncodeToString(k[:]) }
func (k Key) IsZero() bool   { return k == Key{} }

// Short 返回公钥的前几位，用于日志。
func (k Key) Short() string { return k.String()[:8] }

// PublicKey 由私钥算出公钥。
func PublicKey(priv Key) Key {
	pub, _ := curve25519.X25519(priv[:], curve25519.Basepoint)
	var k Key
	copy(k[:], pub)
	return k
}

// Prefix 是 mesh 地址的前缀 fd4e:6167::/32（"Nagi" 的 ASCII）。
var Prefix = netip.MustParsePrefix("fd4e:6167::/32")

// Address 由公钥算出 mesh 地址：前缀加公钥哈希的前 96 位。不需要分配，也无法冒用别人的地址。
func Address(k Key) netip.Addr {
	h := sha256.Sum256(k[:])
	var a [16]byte
	copy(a[:4], []byte{0xfd, 0x4e, 0x61, 0x67})
	copy(a[4:], h[:12])
	return netip.AddrFrom16(a)
}
