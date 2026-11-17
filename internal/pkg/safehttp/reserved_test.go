package safehttp

import (
	"net"
	"net/netip"
	"testing"
)

// L7：保留地址、6to4、IPv4 兼容地址和本机网卡上的地址。
func TestCheckReserved(t *testing.T) {
	cases := []struct {
		addr                 string
		public, allowPrivate bool // 不允许内网时能不能连；允许内网时能不能连
	}{
		{"198.18.0.1", false, true}, // Clash fake-ip：勾了允许内网才能用
		{"192.0.0.8", false, true},
		{"64:ff9b:1::a00:1", false, true},
		{"fec0::1", false, true},
		{"240.0.0.1", false, false},
		{"2002:a00:1::1", false, false},
		{"::127.0.0.1", false, false},
		{"::10.0.0.1", false, true},
		{"::8.8.8.8", true, true},
		{"8.8.8.8", true, true},
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.addr)
		if got := Check(ip, false) == nil; got != c.public {
			t.Errorf("%s 不允许内网：能连 = %v，应为 %v", c.addr, got, c.public)
		}
		if got := Check(ip, true) == nil; got != c.allowPrivate {
			t.Errorf("%s 允许内网：能连 = %v，应为 %v", c.addr, got, c.allowPrivate)
		}
	}
	// 本机网卡上的地址按内网对待（回环本来就始终不允许）。
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, _ := netip.AddrFromSlice(n.IP)
		ip = ip.Unmap()
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if Check(ip, false) == nil {
			t.Errorf("本机地址 %s 不允许内网时不应能连", ip)
		}
	}
}
