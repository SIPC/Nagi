package nodes

import (
	"crypto/rand"
	"fmt"
	"math"
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
	if list[0].NodeRelayEnabled == nil || list[0].GetNodeRelayEnabled() || peers[0].NodeRelayEnabled == nil || *peers[0].NodeRelayEnabled {
		t.Fatal("disabled relay must be explicit, not a legacy missing field")
	}
	c.mesh.Relay = true
	peers, list = r.meshPeers()
	if !list[0].GetNodeRelayEnabled() || peers[0].NodeRelayEnabled == nil || !*peers[0].NodeRelayEnabled {
		t.Fatal("relay permission must be independent of an empty candidate ranking")
	}
	// 中转池默认关闭；面板开了而且节点自己愿意中转才下发。
	if list[0].GetRelayPool() || peers[0].RelayPool {
		t.Fatal("relay pool must default to off")
	}
	r.iso.pool = map[string]bool{"n": true}
	peers, list = r.meshPeers()
	if !list[0].GetRelayPool() || !peers[0].RelayPool {
		t.Fatal("pooled node not announced")
	}
	c.mesh.Relay = false
	peers, list = r.meshPeers()
	if list[0].GetRelayPool() || peers[0].RelayPool {
		t.Fatal("pool must not apply when the node refuses to relay")
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

// v0.35.0#14：被攻破的节点虚报直连延迟（负数、NaN、极小），不能把自己排成第一中转。
// 延迟夹到 [0.1, 10000]，不是有限数的直连不采用；两端都报时取大的。
func TestWithRelaysDistrustsReportedRTT(t *testing.T) {
	a, b, h, x := mesh.PublicKey(randKey()), mesh.PublicKey(randKey()), mesh.PublicKey(randKey()), mesh.PublicKey(randKey())
	direct := func(k mesh.Key, rtt float64) *nagiv1.MeshDirect {
		return &nagiv1.MeshDirect{PublicKey: k[:], RttMs: rtt}
	}
	conn := func(id string, k mesh.Key, directs ...*nagiv1.MeshDirect) *Conn {
		return &Conn{NodeID: id, mesh: &nagiv1.MeshInfo{PublicKey: k[:], Relay: true, Directs: directs}}
	}
	relaysFor := func(r *Registry) []mesh.Key {
		out := r.withRelays(r.conns["a"], []*nagiv1.MeshPeer{{PublicKey: b[:]}})
		var keys []mesh.Key
		for _, rk := range out[0].GetRelays() {
			keys = append(keys, mesh.Key(rk))
		}
		return keys
	}
	// A、B 之间不直连；H 和两端直连（各 50 毫秒）；X 和两端也直连（A、B 报 100 毫秒），但 X 自己报 -1e12。
	r := &Registry{conns: map[string]*Conn{
		"a": conn("a", a, direct(h, 50), direct(x, 100)),
		"b": conn("b", b, direct(h, 50), direct(x, 100)),
		"h": conn("h", h, direct(a, 50), direct(b, 50)),
		"x": conn("x", x, direct(a, -1e12), direct(b, -1e12)),
	}}
	if got := relaysFor(r); len(got) != 2 || got[0] != h || got[1] != x {
		t.Fatalf("虚报负延迟的节点不应排在前面：%v", got)
	}
	// 只有 X 自己报、延迟是 NaN 或无穷：这条直连不采用，X 不能做中转。
	r.conns["a"] = conn("a", a, direct(h, 50))
	r.conns["b"] = conn("b", b, direct(h, 50))
	for _, bad := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		r.conns["x"] = conn("x", x, direct(a, bad), direct(b, bad))
		if got := relaysFor(r); len(got) != 1 || got[0] != h {
			t.Fatalf("延迟 %v 的直连不应采用：%v", bad, got)
		}
	}
	// 只有一端报的直连照常采用，延迟夹到下限，不会是负数。
	r.conns["x"] = conn("x", x, direct(a, -5), direct(b, 0))
	if got := relaysFor(r); len(got) != 2 || got[0] != x {
		t.Fatalf("只有一端报的直连应照常采用：%v", got)
	}
	if v, ok := cleanRTT(-5); !ok || v != minReportedRTT {
		t.Fatalf("负数应夹到 %v：%v", minReportedRTT, v)
	}
	if v, ok := cleanRTT(1e9); !ok || v != maxReportedRTT {
		t.Fatalf("过大的应夹到 %v：%v", maxReportedRTT, v)
	}
}
