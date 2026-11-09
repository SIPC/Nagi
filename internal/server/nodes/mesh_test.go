package nodes

import (
	"crypto/rand"
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
