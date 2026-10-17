package nodes

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// members 返回所有已上报 mesh 信息的在线节点。
func (r *Registry) members() ([]mesh.Peer, []*nagiv1.MeshPeer) {
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
		add := func(ap netip.AddrPort) {
			if ap.IsValid() && ap.Port() != 0 && !slices.Contains(eps, ap) {
				eps = append(eps, ap)
			}
		}
		// 面板看到的节点地址（经过 NAT 以后的公网地址）最有用，放在前面。
		add(direct[k])
		if remote.IsValid() {
			add(netip.AddrPortFrom(remote, uint16(info.GetPort())))
		}
		for _, e := range info.GetEndpoints() {
			if ap, err := netip.ParseAddrPort(e); err == nil {
				add(ap)
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
	peers, _ := r.members()
	if err := m.SetPeers(peers); err != nil {
		r.log.Warn("更新 mesh 成员失败", "err", err)
	}
}

// meshConfig 是告诉节点的成员列表：面板自己加上所有在线节点。
func (r *Registry) meshConfig() *nagiv1.MeshConfig {
	m := r.Mesh()
	if m == nil {
		return nil
	}
	self := m.PublicKey()
	_, list := r.members()
	r.mu.RLock()
	name := r.meshName
	r.mu.RUnlock()
	return &nagiv1.MeshConfig{
		PanelKey: self[:],
		Peers:    append([]*nagiv1.MeshPeer{{PublicKey: self[:], Name: name, IsPanel: true}}, list...),
	}
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
		return mesh.Key{}, errors.New("节点还没有加入 mesh")
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
		return status.Error(codes.Unavailable, "面板的 mesh 没有启动")
	}
	p.conn.mu.RLock()
	info := p.conn.mesh
	p.conn.mu.RUnlock()
	k, err := mesh.ParseKey(info.GetPublicKey())
	if err != nil {
		return status.Error(codes.FailedPrecondition, "请先上报 mesh 信息")
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
		if err := mesh.Serve(ctx, m, nil); err != nil && ctx.Err() == nil {
			r.log.Warn("mesh 服务停止", "err", err)
		}
	}
}
