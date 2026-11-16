package agent

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestBuildRuleset(t *testing.T) {
	st := egressState{
		containers: []egressContainer{
			{project: "aaaa", name: "web", addrs: []netip.Addr{netip.MustParseAddr("172.20.0.2")}},
			{project: "bbbb", name: "db", addrs: []netip.Addr{netip.MustParseAddr("172.21.0.3")}},
			{project: "cccc", name: "off", addrs: []netip.Addr{netip.MustParseAddr("172.22.0.4")}},
		},
		projects: map[string]*nagiv1.Egress{
			"aaaa": {Mode: "block", Allow: []string{"192.168.1.20:5432"}},
			// bbbb 没有设置：升级前的项目，按仅记录。
			"cccc": {Mode: "off"},
		},
		bridges: []string{"br-0123456789ab", "docker0"},
	}
	r := buildRuleset(st)
	for _, want := range []string{
		"table inet nagi {", "elements = { 172.20.0.2 }", "ip saddr @paaaa_4 ip daddr 192.168.1.20/32 meta l4proto { tcp, udp } th dport 5432 accept",
		"ip saddr @paaaa_4 ip daddr @private4 drop", "ip saddr @paaaa_4 ip daddr @local4 drop", `oifname { "br-0123456789ab", "docker0" } accept`,
		"update @hits4 { ip saddr . ip daddr . th dport }", "ct state established,related accept",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("缺少 %q：\n%s", want, r)
		}
	}
	if strings.Contains(r, "@pbbbb_4 ip daddr @private4 drop") {
		t.Error("仅记录的项目不应丢弃")
	}
	if strings.Contains(r, "pcccc") {
		t.Error("关闭的项目不应有规则")
	}
	// 放行在丢弃之前。
	if strings.Index(r, "dport 5432 accept") > strings.Index(r, "@paaaa_4 ip daddr @private4 drop") {
		t.Error("放行应在丢弃之前")
	}
}

func TestParseHits(t *testing.T) {
	out := `table inet nagi {
	set hits4 {
		type ipv4_addr . ipv4_addr . inet_service
		size 65535
		flags dynamic,timeout
		timeout 10m
		elements = { 172.20.0.2 . 169.254.169.254 . 80 expires 9m58s, 172.20.0.2 . 10.0.0.5 . 22 expires 9m}
	}
}`
	hits := parseHits(out)
	if len(hits) != 2 || hits[0].dst.String() != "169.254.169.254" || hits[0].port != 80 || hits[1].port != 22 {
		t.Fatalf("%+v", hits)
	}
}

// M3：从接着拦截模式容器的网桥进来、源地址不是任何容器的地址的连接丢弃（伪造的 IPv4 源地址、IPv6 链路本地地址），
// 在项目规则之前；只管 TCP、UDP；没有这样的网桥时不生成。
func TestBuildRulesetUnknownSource(t *testing.T) {
	st := egressState{
		containers: []egressContainer{
			{project: "aaaa", name: "web", addrs: []netip.Addr{netip.MustParseAddr("172.20.0.2"), netip.MustParseAddr("fd00:20::2")}},
		},
		projects: map[string]*nagiv1.Egress{"aaaa": {Mode: "block"}},
		strict:   []string{"br-0123456789ab", "docker0"},
		known:    []netip.Addr{netip.MustParseAddr("172.20.0.2"), netip.MustParseAddr("172.20.0.9"), netip.MustParseAddr("fd00:20::2")},
	}
	r := buildRuleset(st)
	for _, want := range []string{
		"set known4 {\n\t\ttype ipv4_addr\n\t\telements = { 172.20.0.2, 172.20.0.9 }",
		"set known6 {\n\t\ttype ipv6_addr\n\t\telements = { fd00:20::2 }",
		`iifname { "br-0123456789ab", "docker0" } ip saddr != @known4 ip daddr @private4 meta l4proto { tcp, udp } drop`,
		`iifname { "br-0123456789ab", "docker0" } ip6 saddr != @known6 ip6 daddr @private6 meta l4proto { tcp, udp } drop`,
		`iifname { "br-0123456789ab", "docker0" } ip saddr != @known4 ip daddr @local4 meta l4proto { tcp, udp } drop`,
		`iifname { "br-0123456789ab", "docker0" } ip6 saddr != @known6 ip6 daddr @local6 meta l4proto { tcp, udp } drop`,
		// 容器的 IPv6 地址也进项目规则。
		"elements = { fd00:20::2 }", "ip6 saddr @paaaa_6 ip6 daddr @private6 drop",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("缺少 %q：\n%s", want, r)
		}
	}
	fwd := r[strings.Index(r, "chain forward"):]
	if strings.Index(fwd, "saddr != @known4") > strings.Index(fwd, "@paaaa_4 ip daddr @private4") {
		t.Error("来源不明的丢弃应在项目规则之前")
	}
	st.strict, st.known = nil, nil
	if r := buildRuleset(st); strings.Contains(r, "iifname") {
		t.Errorf("没有拦截模式的网桥时不应有 iifname 规则：\n%s", r)
	}
}

// 容器连节点自己的公网地址上对外开放的端口（访问自己或别的项目的域名）放行，只在 input 链、在项目规则之前；
// 本机的其他端口照样拦截。N1：只放行发往公网地址的，发往局域网 IP、mesh IP、Docker 网关的照样拦截——
// Caddy 转给后端时 X-Forwarded-For 是容器的网桥地址，按私有网段信任来源的后端会把它当成内网访问。
func TestBuildRulesetPublicPorts(t *testing.T) {
	st := egressState{
		containers: []egressContainer{{project: "aaaa", name: "web", addrs: []netip.Addr{netip.MustParseAddr("172.20.0.2")}}},
		projects:   map[string]*nagiv1.Egress{"aaaa": {Mode: "block"}},
		local: []netip.Addr{
			netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("172.17.0.1"), netip.MustParseAddr("100.64.0.9"),
			netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("fd4e::1"), netip.MustParseAddr("fe80::1"),
			netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7"),
		},
		public: []spec.PortKey{
			{Proto: spec.ProtoTCP, Port: 80}, {Proto: spec.ProtoTCP, Port: 443}, {Proto: spec.ProtoTCP, Port: 2222}, {Proto: spec.ProtoUDP, Port: 443},
		},
	}
	r := buildRuleset(st)
	input := r[strings.Index(r, "chain input"):]
	for _, want := range []string{
		"ip daddr { 203.0.113.7 } meta l4proto tcp th dport { 80, 443, 2222 } accept",
		"ip6 daddr { 2001:db8::7 } meta l4proto tcp th dport { 80, 443, 2222 } accept",
		"ip daddr { 203.0.113.7 } meta l4proto udp th dport { 443 } accept",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("input 链缺少 %q：\n%s", want, input)
		}
	}
	for _, line := range strings.Split(input, "\n") {
		if strings.Contains(line, "dport { 80") && !strings.Contains(line, "daddr { 203.0.113.7 }") && !strings.Contains(line, "daddr { 2001:db8::7 }") {
			t.Errorf("放行规则必须限定公网地址：%s", line)
		}
		for _, private := range []string{"192.168.1.5", "172.17.0.1", "100.64.0.9", "fd4e::1", "fe80::1"} {
			if strings.Contains(line, "accept") && strings.Contains(line, private) {
				t.Errorf("发往私有地址 %s 的连接不应放行：%s", private, line)
			}
		}
	}
	if strings.Index(input, "th dport { 80, 443, 2222 } accept") > strings.Index(input, "@paaaa_4 ip daddr @local4 drop") {
		t.Error("公网端口的放行应在项目规则之前")
	}
	forward := r[strings.Index(r, "chain forward"):strings.Index(r, "chain input")]
	if strings.Contains(forward, "dport { 80, 443") {
		t.Error("forward 链不应放行：私有网段里的其他机器不是本机")
	}
	// 节点没有公网地址（在 NAT 后面）：不放行任何本机端口。
	st.local = st.local[:6]
	if r := buildRuleset(st); strings.Contains(r, "dport { 80") {
		t.Errorf("没有公网地址时不应有放行规则：\n%s", r)
	}
}

// L13：forward 链之前放行所有 br-* 出接口。宿主机的局域网网桥恰好叫 br-lan（OpenWrt）、br-ex（OpenStack）时，
// 发往局域网的流量全部放行，出站限制整体失效。现在只放行 Docker 网络的网桥。
func TestBuildRulesetBridgesOnly(t *testing.T) {
	st := egressState{
		containers: []egressContainer{{project: "aaaa", name: "web", addrs: []netip.Addr{netip.MustParseAddr("172.20.0.2")}}},
		projects:   map[string]*nagiv1.Egress{"aaaa": {Mode: "block"}},
		bridges:    []string{"br-0123456789ab"},
	}
	r := buildRuleset(st)
	if strings.Contains(r, `"br-*"`) || strings.Contains(r, "docker0") {
		t.Errorf("不应按 br-* 通配放行：\n%s", r)
	}
	if !strings.Contains(r, `oifname { "br-0123456789ab" } accept`) {
		t.Errorf("应放行容器所在的 Docker 网桥：\n%s", r)
	}
	st.bridges = nil
	if r := buildRuleset(st); strings.Contains(r, "oifname") {
		t.Errorf("没有网桥时不应有放行规则：\n%s", r)
	}
}
