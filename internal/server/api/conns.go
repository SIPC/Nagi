package api

import (
	"context"
	"errors"
	"io"
	"net/netip"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 网络连接（见 docs/agent.md#网络连接）：节点页的「网络连接」实时看节点上的连接和发起的进程。
// 要「查看节点详情」（procedurePerms）；命令行里可能有密码、令牌，另外要「节点终端和文件」并做过高权限确认，
// 没有时拒绝（界面先不要命令行，用户点开时再要）。每次打开记一条审计（WatchNodeConnections，见 activityLog）。

// WatchNodeConnections 把节点的采样结果转给浏览器，浏览器断开时节点停止采样。
func (s *nodeService) WatchNodeConnections(ctx context.Context, req *connect.Request[nagiv1.WatchNodeConnectionsRequest], out *connect.ServerStream[nagiv1.ConnectionSnapshot]) error {
	m := req.Msg
	u := auth.CurrentUser(ctx)
	if m.GetCmdline() {
		if err := s.nodeRootAllowed(ctx, u, m.GetNodeId()); err != nil {
			return connect.NewError(connect.CodePermissionDenied, err)
		}
		if !u.ElevatedFor(nagiv1connect.NodeServiceWatchNodeConnectionsProcedure) {
			return elevationError()
		}
	}
	conn, err := s.Nodes.Get(m.GetNodeId())
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	stream, err := conn.Agent.StreamConnections(ctx, &nagiv1.StreamConnectionsRequest{Cmdline: m.GetCmdline()})
	if err != nil {
		return nodeAccessError(err)
	}
	x, err := s.loadAccess(ctx, u)
	if err != nil {
		return err
	}
	names := s.nodeAddrNames(ctx, x)
	for {
		snap, err := stream.Recv()
		// 浏览器关掉页面是正常结束，审计记成功。
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return nodeAccessError(err)
		}
		cleanConnections(x, names, otherNode(ctx), snap)
		if err := out.Send(snap); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// nodeAddrNames 是各节点的地址（面板看到的来源地址、mesh 地址）到节点 ID 和名称：对端是 Nagi 节点时显示名称。
func (s *nodeService) nodeAddrNames(ctx context.Context, x *access) map[netip.Addr]nodeAddrName {
	out := map[netip.Addr]nodeAddrName{}
	if s.Nodes == nil {
		return out
	}
	for _, c := range s.Nodes.Conns() {
		n := nodeAddrName{id: c.NodeID, name: s.nodeName(ctx, c.NodeID)}
		if ip, ok := c.Source(); ok {
			out[ip.Unmap()] = n
		}
		if r := c.LastReport(); r != nil && len(r.GetMesh().GetPublicKey()) == 32 {
			out[mesh.Address(mesh.Key(r.GetMesh().GetPublicKey()))] = n
		}
	}
	return out
}

type nodeAddrName struct{ id, name string }

// cleanConnections 填上对端节点的名称，并按成员的范围清理：范围外项目的容器不给项目、容器名（Docker 名称里有项目 ID，也去掉），
// 看不到的节点是「其他节点」。x 为 nil（管理员）时只填名称。
func cleanConnections(x *access, names map[netip.Addr]nodeAddrName, other string, snap *nagiv1.ConnectionSnapshot) {
	for _, c := range snap.GetConnections() {
		if ip, err := netip.ParseAddr(c.GetRemoteIp()); err == nil {
			if n, ok := names[ip.Unmap()]; ok {
				c.RemoteNodeName = n.name
				if !x.node(n.id) {
					c.RemoteNodeName = other
				}
			}
		}
		if c.GetProjectId() != "" && !x.project(c.GetProjectId()) {
			c.ProjectId, c.ProjectName, c.Container, c.Service, c.DockerName = "", "", "", "", ""
		}
	}
}
