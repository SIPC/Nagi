package nodes

import (
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func randKey() mesh.Key {
	var k mesh.Key
	_, _ = rand.Read(k[:])
	return k
}

// 节点自己报的地址里，本机、组播、广播等不转给别的节点；面板看到的地址照常转。
func TestMeshPeersFiltersReportedEndpoints(t *testing.T) {
	k := mesh.PublicKey(randKey())
	c := &Conn{NodeID: "n", name: "n", remote: netip.MustParseAddr("127.0.0.1"), mesh: &nagiv1.MeshInfo{
		PublicKey: k[:], Port: 51820,
		Endpoints: []string{"127.0.0.1:1", "[::1]:1", "224.0.0.1:1", "255.255.255.255:1", "[ff02::1]:1",
			"169.254.0.1:1", "0.0.0.0:1", "[" + mesh.Address(k).String() + "]:1", "192.168.1.2:51820", "203.0.113.5:51820"},
	}}
	r := &Registry{conns: map[string]*Conn{"n": c}}
	peers, list := r.meshPeers()
	if len(peers) != 1 || len(list) != 1 {
		t.Fatalf("应有 1 个成员：%d %d", len(peers), len(list))
	}
	want := []string{"127.0.0.1:51820", "192.168.1.2:51820", "203.0.113.5:51820"}
	if !slices.Equal(list[0].Endpoints, want) {
		t.Fatalf("地址不对：%v", list[0].Endpoints)
	}
}

func TestCleanEndpoints(t *testing.T) {
	got, ok := cleanEndpoints([]string{"203.0.113.1:1", "127.0.0.1:1", "x", "[::ffff:203.0.113.1]:1", "203.0.113.2:1"})
	if !ok || !slices.Equal(got, []string{"203.0.113.1:1", "203.0.113.2:1"}) {
		t.Fatalf("应去掉不能用的和重复的：%v %v", got, ok)
	}
	// 多了截掉（IPv6 地址多的机器）。
	var many []string
	for i := range 100 {
		many = append(many, fmt.Sprintf("[2001:db8::%x]:1", i+1))
	}
	if got, ok := cleanEndpoints(many); !ok || len(got) != maxReportedEndpoints || got[0] != "[2001:db8::1]:1" {
		t.Fatalf("应截成前 %d 个：%d %v", maxReportedEndpoints, len(got), ok)
	}
	// 多得离谱的整份不采用。
	many = make([]string, maxReportedRaw+1)
	for i := range many {
		many[i] = "203.0.113.1:1"
	}
	if got, ok := cleanEndpoints(many); ok || len(got) != 0 {
		t.Fatalf("应整份不采用：%d %v", len(got), ok)
	}
}

// 发给节点的每个成员最多 mesh.MaxEndpoints 个地址，面板看到的放在前面。
func TestMeshPeersCapsEndpoints(t *testing.T) {
	k := mesh.PublicKey(randKey())
	var eps []string
	for i := range maxReportedEndpoints {
		eps = append(eps, fmt.Sprintf("203.0.113.%d:1", i+1))
	}
	c := &Conn{NodeID: "n", remote: netip.MustParseAddr("198.51.100.1"), mesh: &nagiv1.MeshInfo{PublicKey: k[:], Port: 2, Endpoints: eps}}
	r := &Registry{conns: map[string]*Conn{"n": c}}
	_, list := r.meshPeers()
	if got := list[0].Endpoints; len(got) != mesh.MaxEndpoints || got[0] != "198.51.100.1:2" {
		t.Fatalf("应最多 %d 个、面板看到的在前：%v", mesh.MaxEndpoints, got)
	}
}

// 两端直连时不给中转节点；不直连时只挑和两端都直连、愿意中转的节点，按两段延迟之和从低到高，最多两个。
func TestPickRelays(t *testing.T) {
	k := func(b byte) mesh.Key { return mesh.Key{b} }
	la, jp, sg, hk, cn, off := k(1), k(2), k(3), k(4), k(5), k(6)
	links := map[mesh.Key]map[mesh.Key]float64{}
	add := func(a, b mesh.Key, rtt float64) {
		for _, x := range [][2]mesh.Key{{a, b}, {b, a}} {
			if links[x[0]] == nil {
				links[x[0]] = map[mesh.Key]float64{}
			}
			links[x[0]][x[1]] = rtt
		}
	}
	add(la, sg, 200)
	add(sg, jp, 70)
	add(la, hk, 199)
	add(hk, jp, 50)
	add(la, cn, 166) // CN 和 JP 不直连，不能中转
	add(la, off, 10)
	add(off, jp, 10) // 最近，但关了中转
	add(sg, hk, 30)
	relayOK := map[mesh.Key]bool{la: true, jp: true, sg: true, hk: true, cn: true, off: false}
	got := pickRelays(links, relayOK, la, jp)
	if len(got) != 2 || got[0] != hk || got[1] != sg {
		t.Fatalf("LA 到 JP 的中转节点 %v，应该是 HK、SG", got)
	}
	if got := pickRelays(links, relayOK, sg, hk); got != nil {
		t.Fatalf("直连的两端不该有中转节点：%v", got)
	}
	if got := pickRelays(links, relayOK, cn, jp); len(got) != 0 {
		t.Fatalf("CN 到 JP 没有和两端都直连的节点（LA 和 JP 不直连）：%v", got)
	}
}
