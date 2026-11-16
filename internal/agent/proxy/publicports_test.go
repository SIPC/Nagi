package proxy

import (
	"slices"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
)

// N1：出站限制只放行 Caddy 实际监听的端口。没有上游的 TCP/UDP 规则（容器没运行）Caddy 不监听，不能放行：
// 之后占用这个端口的别的程序也会被容器连到。
func TestPublicPortsOnlyListening(t *testing.T) {
	got := publicPorts([]Route{
		{Protocol: spec.ProtoHTTP, Domains: []string{"a.example.com"}},
		{Protocol: spec.ProtoTCP, Published: 2222, Upstreams: []string{"10.200.0.2:22"}},
		{Protocol: spec.ProtoTCP, Published: 3333},
		{Protocol: spec.ProtoUDP, Published: 5353},
	})
	want := []spec.PortKey{{Proto: spec.ProtoTCP, Port: 80}, {Proto: spec.ProtoTCP, Port: 443}, {Proto: spec.ProtoUDP, Port: 443}, {Proto: spec.ProtoTCP, Port: 2222}}
	if !slices.Equal(got, want) {
		t.Fatalf("publicPorts = %v，应该是 %v", got, want)
	}
}
