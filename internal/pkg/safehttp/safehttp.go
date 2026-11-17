// Package safehttp 是按别人给的网址发请求时用的 HTTP 客户端（告警 webhook、OAuth 客户端元数据）：
// 只连接允许的地址，不能被拿来访问本机、内网和云服务器的元数据服务（SSRF）。
//
// 地址在连接时检查：域名先照常解析，拨号前逐个检查要连接的 IP（net.Dialer.Control），
// 所以 DNS 重绑定（检查时解析到公网、连接时解析到内网）也绕不过去。
// 不用环境变量里的代理：经代理时连接的是代理，目的地址检查不到。也不跟随跳转。
package safehttp

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// BlockedError 是因为地址不允许而没有连接。
type BlockedError struct {
	IP netip.Addr
	// Private：是内网地址，允许内网地址时可以连接。为 false 时（本机、链路本地、元数据地址等）任何情况都不能连接。
	Private bool
}

func (e *BlockedError) Error() string {
	if e.Private {
		return "private address " + e.IP.String() + " is not allowed"
	}
	return "address " + e.IP.String() + " is not allowed"
}

var (
	// cgnat 是运营商 NAT 的地址（100.64.0.0/10），也常用作 VPN（例如 Tailscale）的内网地址。
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	// thisNet 是 0.0.0.0/8：「本网络」，有的系统上连它等于连本机。
	thisNet = netip.MustParsePrefix("0.0.0.0/8")
	// nat64 里嵌着 IPv4 地址，按嵌着的地址判断。
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	// metadata 是云服务器的元数据服务和宿主机服务的地址，允许内网地址时也不能连接。
	// 169.254.169.254（AWS、GCP、Azure、阿里云以外的多数云）、169.254.0.23（腾讯云）属于链路本地，已经拦下。
	metadata = []netip.Addr{
		netip.MustParseAddr("100.100.100.200"), // 阿里云
		netip.MustParseAddr("fd00:ec2::254"),   // AWS IPv6
		netip.MustParseAddr("168.63.129.16"),   // Azure WireServer
	}
	broadcast = netip.MustParseAddr("255.255.255.255")
)

// Check 返回能不能连接 ip：不能时返回 *BlockedError。allowPrivate 为 true 时允许内网地址
// （10.0.0.0/8、172.16.0.0/12、192.168.0.0/16、100.64.0.0/10、fc00::/7），本机、未指定、链路本地、
// 组播、广播和云服务器的元数据地址始终不允许。
func Check(ip netip.Addr, allowPrivate bool) error {
	ip = ip.WithZone("").Unmap()
	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip == broadcast || thisNet.Contains(ip) {
		return &BlockedError{IP: ip}
	}
	for _, m := range metadata {
		if ip == m {
			return &BlockedError{IP: ip}
		}
	}
	if ip.IsPrivate() || cgnat.Contains(ip) {
		if allowPrivate {
			return nil
		}
		return &BlockedError{IP: ip, Private: true}
	}
	return nil
}

// Dialer 返回只连接 Check 允许的地址的拨号器。
func Dialer(timeout time.Duration, allowPrivate bool) *net.Dialer {
	return dialer(timeout, func(ip netip.Addr) error { return Check(ip, allowPrivate) })
}

func dialer(timeout time.Duration, check func(netip.Addr) error) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		// Control 在解析完、真正连接每个地址之前调用，address 是 IP:端口。
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return &BlockedError{}
			}
			return check(ap.Addr())
		},
	}
}

// NoRedirect 不跟随跳转，直接返回 3xx 响应：跳转的目标不能由对方决定。
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Client 返回只连接 Check 允许的地址的客户端：不用代理，不跟随跳转，整个请求最多 timeout。
func Client(timeout time.Duration, allowPrivate bool) *http.Client {
	return client(timeout, func(ip netip.Addr) error { return Check(ip, allowPrivate) })
}

func client(timeout time.Duration, check func(netip.Addr) error) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = dialer(min(timeout, 10*time.Second), check).DialContext
	t.DialTLSContext = nil
	return &http.Client{Timeout: timeout, CheckRedirect: NoRedirect, Transport: t}
}

// Blocked 返回 err 是不是因为地址不允许，以及是哪个地址。
func Blocked(err error) (*BlockedError, bool) {
	var b *BlockedError
	if errors.As(err, &b) {
		return b, true
	}
	return nil, false
}
