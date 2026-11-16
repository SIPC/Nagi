package agent

import (
	"net/netip"
	"strings"
	"testing"

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
	}
	r := buildRuleset(st)
	for _, want := range []string{
		"table inet nagi {", "elements = { 172.20.0.2 }", "ip saddr @paaaa_4 ip daddr 192.168.1.20/32 meta l4proto { tcp, udp } th dport 5432 accept",
		"ip saddr @paaaa_4 ip daddr @private4 drop", "ip saddr @paaaa_4 ip daddr @local4 drop", `oifname { "docker0", "br-*" } accept`,
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
