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
