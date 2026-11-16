package spec

import (
	"net"
	"net/netip"
	"strconv"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 容器出站限制（见 docs/agent.md#出站限制）。

// 出站限制的模式。
const (
	EgressBlock = "block"
	EgressLog   = "log"
	EgressOff   = "off"
)

// EgressMode 返回生效的模式：没有设置的（升级前创建的项目）按仅记录。
func EgressMode(e *nagiv1.Egress) string {
	switch m := e.GetMode(); m {
	case EgressBlock, EgressOff:
		return m
	}
	return EgressLog
}

// EgressRule 是一条放行：网段和端口（0 表示所有端口）。
type EgressRule struct {
	Prefix netip.Prefix
	Port   uint16
}

// ParseEgressAllow 解析一条放行：<地址或网段>:<端口>，IPv6 写成 [地址]:端口 或 [网段]:端口。
func ParseEgressAllow(s string) (EgressRule, error) {
	s = strings.TrimSpace(s)
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return EgressRule{}, Invalid("放行的地址 %s 格式不正确，写成 地址:端口，例如 192.168.1.20:5432", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return EgressRule{}, Invalid("放行的地址 %s 的端口须在 1–65535 之间", s)
	}
	var p netip.Prefix
	if strings.Contains(host, "/") {
		p, err = netip.ParsePrefix(host)
	} else {
		var a netip.Addr
		if a, err = netip.ParseAddr(host); err == nil {
			p = netip.PrefixFrom(a, a.BitLen())
		}
	}
	if err != nil {
		return EgressRule{}, Invalid("放行的地址 %s 不是 IP 地址或网段", s)
	}
	return EgressRule{Prefix: p.Masked(), Port: uint16(n)}, nil
}

// String 写回 <地址或网段>:<端口>。
func (r EgressRule) String() string {
	host := r.Prefix.Addr().String()
	if r.Prefix.Bits() != r.Prefix.Addr().BitLen() {
		host = r.Prefix.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(int(r.Port)))
}

// CleanEgress 检查并整理出站限制，nil 表示没有设置。
func CleanEgress(e *nagiv1.Egress) (*nagiv1.Egress, error) {
	if e == nil {
		return nil, nil
	}
	out := &nagiv1.Egress{Mode: e.GetMode()}
	switch out.Mode {
	case EgressBlock, EgressLog, EgressOff:
	case "":
		out.Mode = EgressLog
	default:
		return nil, Invalid("出站限制的模式只能是 block、log 或 off")
	}
	if len(e.GetAllow()) > 100 {
		return nil, Invalid("放行的地址最多 100 条")
	}
	seen := map[string]bool{}
	for _, a := range e.GetAllow() {
		if strings.TrimSpace(a) == "" {
			continue
		}
		r, err := ParseEgressAllow(a)
		if err != nil {
			return nil, err
		}
		if s := r.String(); !seen[s] {
			seen[s] = true
			out.Allow = append(out.Allow, s)
		}
	}
	return out, nil
}

// EgressEqual 判断两份出站限制是否一样（没有设置和仅记录、没有放行算一样）。
func EgressEqual(a, b *nagiv1.Egress) bool {
	if EgressMode(a) != EgressMode(b) || len(a.GetAllow()) != len(b.GetAllow()) {
		return false
	}
	for i := range a.GetAllow() {
		if a.GetAllow()[i] != b.GetAllow()[i] {
			return false
		}
	}
	return true
}
