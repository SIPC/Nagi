package nodes

import (
	"bytes"
	"cmp"
	"context"
	"net/netip"
	"slices"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 面板也是 mesh 成员，同时替自己的节点中转：UDP 中转用面板端口（UDP），TLS 中转走控制连接。
// 面板把自己和所有在线节点作为成员列表，在每次上报的响应里告诉节点。

// SetMesh 设置面板的 mesh。name 是面板在成员列表里的显示名称。
func (r *Registry) SetMesh(m *mesh.Mesh, name string) {
	r.mu.Lock()
	r.mesh, r.meshName = m, name
	r.mu.Unlock()
}

// Mesh 返回面板的 mesh，没有启动时为 nil。
func (r *Registry) Mesh() *mesh.Mesh {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mesh
}

const (
	// maxReportedEndpoints 是节点上报的直连地址最多采用几个。IPv6 地址多的机器可能超过 16 个，多的截掉。
	maxReportedEndpoints = 32
	// maxReportedRaw：上报的地址超过这么多个时整份不采用，正常节点不会报这么多。
	maxReportedRaw = 256
)

// cleanEndpoints 整理节点上报的直连地址：去掉不能用的和重复的，最多留 maxReportedEndpoints 个。
// 超过 maxReportedRaw 个时一个都不留，ok 为 false。
func cleanEndpoints(raw []string) (out []string, ok bool) {
	if len(raw) > maxReportedRaw {
		return nil, false
	}
	seen := make(map[netip.AddrPort]bool, len(raw))
	for _, e := range raw {
		ap, err := netip.ParseAddrPort(e)
		if err != nil || !mesh.UsableEndpoint(ap) {
			continue
		}
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if seen[ap] {
			continue
		}
		seen[ap] = true
		out = append(out, ap.String())
		if len(out) == maxReportedEndpoints {
			break
		}
	}
	return out, true
}

// meshPeers 返回所有已上报 mesh 信息的在线节点。
func (r *Registry) meshPeers() ([]mesh.Peer, []*nagiv1.MeshPeer) {
	m := r.Mesh()
	direct := map[mesh.Key]netip.AddrPort{}
	if m != nil {
		for _, s := range m.Status() {
			if s.Direct.IsValid() {
				direct[s.Key] = s.Direct
			}
		}
	}
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()

	var peers []mesh.Peer
	var list []*nagiv1.MeshPeer
	for _, c := range conns {
		c.mu.RLock()
		info, name, remote := c.mesh, c.name, c.remote
		c.mu.RUnlock()
		k, err := mesh.ParseKey(info.GetPublicKey())
		if err != nil {
			continue
		}
		var eps []netip.AddrPort
		seen := map[netip.AddrPort]bool{}
		add := func(ap netip.AddrPort) {
			if ap.IsValid() && ap.Port() != 0 && !seen[ap] && len(eps) < mesh.MaxEndpoints {
				seen[ap] = true
				eps = append(eps, ap)
			}
		}
		// 面板看到的节点地址（经过 NAT 以后的公网地址）最有用，放在前面。
		add(direct[k])
		if remote.IsValid() {
			add(netip.AddrPortFrom(remote, uint16(info.GetPort())))
		}
		// 节点自己报的地址不可信，只留能用来直连的；上面两个是面板看到的，不过滤（自带节点经本机连接）。
		for _, e := range info.GetEndpoints() {
			if ap, err := netip.ParseAddrPort(e); err == nil && mesh.UsableEndpoint(ap) {
				add(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
			}
		}
		peers = append(peers, mesh.Peer{Key: k, Endpoints: eps})
		mp := &nagiv1.MeshPeer{PublicKey: k[:], Name: name, NodeId: c.NodeID}
		for _, e := range eps {
			mp.Endpoints = append(mp.Endpoints, e.String())
		}
		list = append(list, mp)
	}
	return peers, list
}

// updateMesh 按在线节点更新面板自己的 mesh 成员。
func (r *Registry) updateMesh() {
	m := r.Mesh()
	if m == nil {
		return
	}
	peers, _ := r.meshPeers()
	if err := m.SetPeers(peers); err != nil {
		r.log.Warn("更新 mesh 成员失败", "err", err)
	}
}

// meshConfig 是告诉节点 to 的成员列表：面板自己加上所有在线节点。和 to 直连不通的节点，带上替 to 中转的节点。
func (r *Registry) meshConfig(to *Conn) *nagiv1.MeshConfig {
	m := r.Mesh()
	if m == nil {
		return nil
	}
	self := m.PublicKey()
	_, list := r.meshPeers()
	r.mu.RLock()
	name := r.meshName
	r.mu.RUnlock()
	if to != nil {
		list = r.withRelays(to, list)
	}
	return &nagiv1.MeshConfig{
		PanelKey: self[:],
		Peers:    append([]*nagiv1.MeshPeer{{PublicKey: self[:], Name: name, IsPanel: true}}, list...),
	}
}

// maxNodeRelays 是每对节点最多给几个中转节点：第一个不通时节点自己换下一个，都不通再经面板。
const maxNodeRelays = 2

// withRelays 给成员列表里和 to 直连不通的节点挑中转节点（见 docs/mesh.md#经节点中转）。按各节点上报的直连情况算，
// 不改 list 里原来的条目。
func (r *Registry) withRelays(to *Conn, list []*nagiv1.MeshPeer) []*nagiv1.MeshPeer {
	to.mu.RLock()
	from, err := mesh.ParseKey(to.mesh.GetPublicKey())
	to.mu.RUnlock()
	if err != nil {
		return list
	}
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	links := map[mesh.Key]map[mesh.Key]float64{}
	relayOK := map[mesh.Key]bool{}
	link := func(a, b mesh.Key, rtt float64) {
		if links[a] == nil {
			links[a] = map[mesh.Key]float64{}
		}
		if old, ok := links[a][b]; !ok || rtt < old {
			links[a][b] = rtt
		}
	}
	for _, c := range conns {
		c.mu.RLock()
		info := c.mesh
		pending := c.pending
		c.mu.RUnlock()
		k, err := mesh.ParseKey(info.GetPublicKey())
		if err != nil || pending {
			continue
		}
		relayOK[k] = info.GetRelay()
		for i, d := range info.GetDirects() {
			if i >= maxReportedRaw {
				break
			}
			if p, err := mesh.ParseKey(d.GetPublicKey()); err == nil && p != k {
				// 任一端报了直连就算通，两端报的延迟取小的。
				link(k, p, d.GetRttMs())
				link(p, k, d.GetRttMs())
			}
		}
	}
	out := make([]*nagiv1.MeshPeer, 0, len(list))
	for _, mp := range list {
		k, err := mesh.ParseKey(mp.GetPublicKey())
		if err != nil || k == from {
			out = append(out, mp)
			continue
		}
		relays := pickRelays(links, relayOK, from, k)
		if len(relays) == 0 {
			out = append(out, mp)
			continue
		}
		x := proto.CloneOf(mp)
		for _, rk := range relays {
			x.Relays = append(x.Relays, rk[:])
		}
		out = append(out, x)
	}
	return out
}

// pickRelays 给 from 到 to 挑中转节点：两端之间没有直连时，找和两端都直连、愿意中转的节点，
// 按两段延迟之和从低到高，最多 maxNodeRelays 个。links[a][b] 是 a 和 b 直连的延迟（毫秒）。
func pickRelays(links map[mesh.Key]map[mesh.Key]float64, relayOK map[mesh.Key]bool, from, to mesh.Key) []mesh.Key {
	if _, ok := links[from][to]; ok {
		return nil
	}
	type cand struct {
		k   mesh.Key
		rtt float64
	}
	var cands []cand
	for rk, ok := range relayOK {
		if !ok || rk == from || rk == to {
			continue
		}
		a, ok1 := links[from][rk]
		b, ok2 := links[rk][to]
		if ok1 && ok2 {
			cands = append(cands, cand{rk, a + b})
		}
	}
	slices.SortFunc(cands, func(x, y cand) int {
		if x.rtt != y.rtt {
			return cmp.Compare(x.rtt, y.rtt)
		}
		return bytes.Compare(x.k[:], y.k[:])
	})
	var out []mesh.Key
	for _, c := range cands[:min(len(cands), maxNodeRelays)] {
		out = append(out, c.k)
	}
	return out
}

// NodeMeshKey 返回在线节点的 mesh 公钥。
func (r *Registry) NodeMeshKey(nodeID string) (mesh.Key, error) {
	c, err := r.Get(nodeID)
	if err != nil {
		return mesh.Key{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, err := mesh.ParseKey(c.mesh.GetPublicKey())
	if err != nil {
		return mesh.Key{}, i18n.Errorf("节点 %s 还没有加入节点网络", c.name)
	}
	return k, nil
}

// MeshPeerName 返回 mesh 成员的显示名称。
func (r *Registry) MeshPeerName(k mesh.Key) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.conns {
		c.mu.RLock()
		match := c.mesh != nil && slices.Equal(c.mesh.GetPublicKey(), k[:])
		name := c.name
		c.mu.RUnlock()
		if match {
			return name
		}
	}
	return k.Short()
}

// MeshRelay 是节点的 TLS 中转通道。发送方一律按这条连接所属的节点算，不信任包里写的来源。
func (p *panelService) MeshRelay(stream nagiv1.PanelService_MeshRelayServer) error {
	m := p.r.Mesh()
	if m == nil {
		return status.Error(codes.Unavailable, "面板的节点网络没有启动")
	}
	p.conn.mu.RLock()
	info := p.conn.mesh
	p.conn.mu.RUnlock()
	k, err := mesh.ParseKey(info.GetPublicKey())
	if err != nil {
		return status.Error(codes.FailedPrecondition, "请先上报节点网络信息")
	}
	var mu sync.Mutex
	detach := m.AttachLink(k, func(src, dst mesh.Key, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(&nagiv1.MeshPacket{Src: src[:], Dst: dst[:], Payload: payload})
	})
	defer detach()
	for {
		pkt, err := stream.Recv()
		if err != nil {
			return nil
		}
		if dst, err := mesh.ParseKey(pkt.GetDst()); err == nil {
			m.HandleLink(k, dst, pkt.GetPayload())
		}
	}
}

// ServeMesh 在面板的 mesh 地址上提供 MeshService，直到 ctx 取消。
func (r *Registry) ServeMesh(ctx context.Context) {
	if m := r.Mesh(); m != nil {
		if err := mesh.Serve(ctx, m, &panelMesh{Service: mesh.Service{M: m}, r: r}); err != nil && ctx.Err() == nil {
			r.log.Warn("mesh 服务停止", "err", err)
		}
	}
}

// NodeByMeshKey 返回 mesh 公钥是 k 的在线节点。
func (r *Registry) NodeByMeshKey(k []byte) (*Conn, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.conns {
		c.mu.RLock()
		match := c.mesh != nil && slices.Equal(c.mesh.GetPublicKey(), k)
		name := c.name
		c.mu.RUnlock()
		if match {
			return c, name, true
		}
	}
	return nil, "", false
}

// Name 返回节点的显示名称。
func (c *Conn) Name() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.name
}

// Remote 返回节点这条连接的来源地址。
func (c *Conn) Remote() netip.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remote
}
