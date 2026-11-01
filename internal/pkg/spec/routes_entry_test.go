package spec

import (
	"slices"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestEntryNodesOf(t *testing.T) {
	cases := []struct {
		name string
		r    *nagiv1.Exposure
		want []string
	}{
		{"默认只在容器所在节点", &nagiv1.Exposure{}, []string{"a"}},
		{"再加一个入口", &nagiv1.Exposure{EntryNodes: []string{"b"}}, []string{"a", "b"}},
		{"只从别的节点进", &nagiv1.Exposure{EntryNodes: []string{"b"}, NoLocalEntry: true}, []string{"b"}},
		{"容器所在节点同时是固定入口时不重复", &nagiv1.Exposure{EntryNodes: []string{"a", "b"}}, []string{"a", "b"}},
	}
	for _, c := range cases {
		if got := EntryNodesOf(c.r, "a"); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeRouteKeepsEntries(t *testing.T) {
	r := NormalizeRoute(&nagiv1.Exposure{Container: "web", Target: 80, Protocol: "tcp", Published: 8080,
		EntryNodes: []string{" b ", "a", "b", ""}, NoLocalEntry: true})
	if !slices.Equal(r.GetEntryNodes(), []string{"a", "b"}) || !r.GetNoLocalEntry() {
		t.Fatalf("got %v %v", r.GetEntryNodes(), r.GetNoLocalEntry())
	}
}

func TestListenPorts(t *testing.T) {
	cases := []struct {
		name string
		r    *nagiv1.Exposure
		want []PortKey
	}{
		{"http 没绑域名", &nagiv1.Exposure{Protocol: ProtoHTTP, Published: 8080}, []PortKey{{ProtoTCP, 8080}}},
		{"tcp", &nagiv1.Exposure{Protocol: ProtoTCP, Published: 5432}, []PortKey{{ProtoTCP, 5432}}},
		{"udp", &nagiv1.Exposure{Protocol: ProtoUDP, Published: 53}, []PortKey{{ProtoUDP, 53}}},
		{"绑了域名", &nagiv1.Exposure{Protocol: ProtoHTTP, Domains: []string{"a.example.com"}}, []PortKey{{ProtoTCP, 80}, {ProtoTCP, 443}}},
		{"只开 HTTP", &nagiv1.Exposure{Protocol: ProtoHTTP, Domains: []string{"a.example.com"}, HttpOnly: true}, []PortKey{{ProtoTCP, 80}}},
	}
	for _, c := range cases {
		if got := ListenPorts(c.r); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCheckRouteNeedsAnEntry(t *testing.T) {
	r := &nagiv1.Exposure{Container: "web", Service: "web", Target: 80, Protocol: "tcp", Published: 8080}
	if err := CheckRoute(r); err != nil {
		t.Fatal(err)
	}
	r.NoLocalEntry = true
	if CheckRoute(r) == nil {
		t.Fatal("没有入口节点时应该报错")
	}
	r.EntryNodes = []string{"b"}
	if err := CheckRoute(r); err != nil {
		t.Fatal(err)
	}
}
