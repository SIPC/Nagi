package spec

import (
	"slices"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// tcp+udp 占用同一个端口的 TCP 和 UDP，下发时拆成两条同名的映射，从节点拼回时合成一条。
func TestTCPUDPRoute(t *testing.T) {
	r := &nagiv1.Exposure{Name: "mc", Container: "mc", Service: "mc", Target: 25565, Protocol: ProtoTCPUDP, Published: 25565, ProxyProtocol: true}
	if err := CheckRoute(r); err != nil {
		t.Fatal(err)
	}
	if got := UsedPorts(r); !slices.Equal(got, []PortKey{{ProtoTCP, 25565}, {ProtoUDP, 25565}}) {
		t.Fatalf("UsedPorts = %v", got)
	}
	parts := SplitRoute(r)
	if len(parts) != 2 || parts[0].GetProtocol() != ProtoTCP || parts[1].GetProtocol() != ProtoUDP || r.GetProtocol() != ProtoTCPUDP {
		t.Fatalf("SplitRoute = %v", parts)
	}
	for _, p := range parts {
		if p.GetName() != "mc" || !p.GetProxyProtocol() || CheckRoute(p) != nil {
			t.Fatalf("half = %v", p)
		}
	}
	other := &nagiv1.Exposure{Name: "web", Container: "web", Service: "web", Target: 80, Protocol: ProtoHTTP, Published: 8080}
	joined := JoinRoutes(append(parts, other))
	if len(joined) != 2 || !proto.Equal(joined[0], r) || !proto.Equal(joined[1], other) {
		t.Fatalf("JoinRoutes = %v", joined)
	}
	if got := SplitRoute(other); len(got) != 1 || got[0] != other {
		t.Fatalf("SplitRoute(http) = %v", got)
	}
}

// 同名但设置不同的 tcp 和 udp（不是拆出来的）不合并。
func TestJoinRoutesKeepsDifferent(t *testing.T) {
	a := &nagiv1.Exposure{Name: "x", Container: "c", Service: "c", Target: 1, Protocol: ProtoTCP, Published: 1000}
	b := &nagiv1.Exposure{Name: "x", Container: "c", Service: "c", Target: 2, Protocol: ProtoUDP, Published: 1000}
	if got := JoinRoutes([]*nagiv1.Exposure{a, b}); len(got) != 2 {
		t.Fatalf("JoinRoutes = %v", got)
	}
}

func TestProxyProtocolOnlyForL4(t *testing.T) {
	r := &nagiv1.Exposure{Container: "web", Service: "web", Target: 80, Protocol: ProtoHTTP, Published: 8080, ProxyProtocol: true}
	if CheckRoute(r) == nil {
		t.Fatal("http 映射不能开 PROXY protocol")
	}
	r.Domains = []string{"example.com"}
	if CheckRoute(NormalizeRoute(r)) == nil {
		t.Fatal("绑了域名的 http 映射也不能开")
	}
	if !NormalizeRoute(&nagiv1.Exposure{ProxyProtocol: true}).GetProxyProtocol() {
		t.Fatal("NormalizeRoute 丢了 ProxyProtocol")
	}
}
