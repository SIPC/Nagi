package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 跨节点连接共享网络（见 docs/agent.md#跨节点连接共享网络）：连接关系存在各节点上，面板只负责
// 把一组节点同时设置好。两组已经连通的节点连在一起时合并成一组，组里每两个节点之间都相连。

type linkMember struct {
	conn   *nodes.Conn
	key    []byte
	name   string
	nodeID string
	net    *nagiv1.SharedNetwork // 这个节点上的网络，没有时为 nil
}

func findNetwork(list []*nagiv1.SharedNetwork, name string) *nagiv1.SharedNetwork {
	for _, n := range list {
		if n.GetName() == name {
			return n
		}
	}
	return nil
}

// linkMemberOf 读取一个在线节点上的网络 name。
func (s *nodeService) linkMemberOf(ctx context.Context, conn *nodes.Conn, name string) (*linkMember, error) {
	key, err := s.Nodes.NodeMeshKey(conn.NodeID)
	if err != nil {
		return nil, fmt.Errorf("节点 %s：%w", conn.Name(), err)
	}
	resp, err := conn.Agent.ListSharedNetworks(ctx, &nagiv1.ListSharedNetworksRequest{})
	if err != nil {
		return nil, fmt.Errorf("读取节点 %s 的共享网络：%w", conn.Name(), fromGRPC(err))
	}
	return &linkMember{conn: conn, key: key[:], name: conn.Name(), nodeID: conn.NodeID, net: findNetwork(resp.GetNetworks(), name)}, nil
}

func (s *nodeService) LinkNodeSharedNetwork(ctx context.Context, req *connect.Request[nagiv1.LinkNodeSharedNetworkRequest]) (*connect.Response[nagiv1.ListSharedNetworksResponse], error) {
	name := req.Msg.GetName()
	if req.Msg.GetNodeId() == req.Msg.GetPeerNodeId() {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("请选另一个节点"))
	}
	group := []*linkMember{}
	has := func(k []byte) bool {
		for _, m := range group {
			if bytes.Equal(m.key, k) {
				return true
			}
		}
		return false
	}
	for _, id := range []string{req.Msg.GetNodeId(), req.Msg.GetPeerNodeId()} {
		conn, err := s.Nodes.Get(id)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		m, err := s.linkMemberOf(ctx, conn, name)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		if m.net == nil && id == req.Msg.GetNodeId() {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("节点 %s 上没有共享网络 %s", m.name, name))
		}
		if !has(m.key) {
			group = append(group, m)
		}
	}
	// 两边原来已经相连的节点一起并进来。它们都必须由这个面板管理并且在线。
	for i := 0; i < len(group); i++ {
		for _, p := range group[i].net.GetPeers() {
			if has(p.GetPublicKey()) {
				continue
			}
			conn, _, ok := s.Nodes.NodeByMeshKey(p.GetPublicKey())
			if !ok {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"节点 %s 上的 %s 还和节点 %s 相连，它不在线或者不归这个面板管理；要合并的组里所有节点都必须在这个面板下并且在线", group[i].name, name, p.GetName()))
			}
			m, err := s.linkMemberOf(ctx, conn, name)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, err)
			}
			group = append(group, m)
		}
	}
	// 网络名在整组里唯一。
	type use struct{ project, projectName, node string }
	uses := map[string]use{}
	for _, m := range group {
		for _, p := range m.net.GetProjects() {
			if prev, ok := uses[p.GetNetworkName()]; ok && prev.project != p.GetProjectId() {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"网络名 %s 在节点 %s（项目 %s）和节点 %s（项目 %s）上都有，连起来会撞名，请先改掉其中一个项目的网络名",
					p.GetNetworkName(), prev.node, prev.projectName, m.name, p.GetProjectName()))
			}
			uses[p.GetNetworkName()] = use{p.GetProjectId(), p.GetProjectName(), m.name}
		}
	}
	var first *nagiv1.ListSharedNetworksResponse
	for _, m := range group {
		var peers []*nagiv1.SharedNetworkPeer
		for _, o := range group {
			if o != m {
				peers = append(peers, &nagiv1.SharedNetworkPeer{PublicKey: o.key, Name: o.name, NodeId: o.nodeID})
			}
		}
		resp, err := m.conn.Agent.SetSharedNetworkPeers(ctx, &nagiv1.SetSharedNetworkPeersRequest{Name: name, Peers: peers})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("设置节点 %s：%w（可以重试，设置是幂等的）", m.name, fromGRPC(err)))
		}
		if m.nodeID == req.Msg.GetNodeId() {
			first = resp
		}
	}
	return connect.NewResponse(first), nil
}

func (s *nodeService) UnlinkNodeSharedNetwork(ctx context.Context, req *connect.Request[nagiv1.NodeSharedNetworkRequest]) (*connect.Response[nagiv1.ListSharedNetworksResponse], error) {
	name := req.Msg.GetName()
	conn, err := s.Nodes.Get(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	self, err := s.linkMemberOf(ctx, conn, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if self.net == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("节点上没有共享网络 %s", name))
	}
	// 组里其他节点删掉本节点。联系不上的节点会在下次查询时被本节点拒绝，自己删掉本节点的代理。
	for _, p := range self.net.GetPeers() {
		c, _, ok := s.Nodes.NodeByMeshKey(p.GetPublicKey())
		if !ok {
			continue
		}
		m, err := s.linkMemberOf(ctx, c, name)
		if err != nil || m.net == nil {
			continue
		}
		var keep []*nagiv1.SharedNetworkPeer
		for _, q := range m.net.GetPeers() {
			if !bytes.Equal(q.GetPublicKey(), self.key) {
				keep = append(keep, &nagiv1.SharedNetworkPeer{PublicKey: q.GetPublicKey(), Name: q.GetName(), NodeId: q.GetNodeId()})
			}
		}
		_, _ = c.Agent.SetSharedNetworkPeers(ctx, &nagiv1.SetSharedNetworkPeersRequest{Name: name, Peers: keep})
	}
	resp, err := conn.Agent.SetSharedNetworkPeers(ctx, &nagiv1.SetSharedNetworkPeersRequest{Name: name})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}
