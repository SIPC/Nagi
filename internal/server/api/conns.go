package api

import (
	"context"
	"errors"
	"io"
	"net/netip"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
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
	names := s.nagiAddrs(ctx)
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

// nagiAddrs 是 Nagi 自己的地址到节点 ID 和名称：各节点面板看到的来源地址和 mesh 地址，面板的对外地址和 mesh 地址
// （id 为空，名称是「面板」）。对端或本端是它们时显示名称，地址要「查看访问地址」才给。
func (s *nodeService) nagiAddrs(ctx context.Context) map[netip.Addr]nodeAddrName {
	out := map[netip.Addr]nodeAddrName{}
	panel := i18n.T(ctx, "面板")
	if s.Panel != nil {
		if ip, err := netip.ParseAddr(s.Panel.PublicAddress(ctx)); err == nil && !ip.IsLoopback() {
			out[ip.Unmap()] = nodeAddrName{name: panel}
		}
	}
	if s.Nodes == nil {
		return out
	}
	if m := s.Nodes.Mesh(); m != nil {
		out[mesh.Address(m.PublicKey())] = nodeAddrName{name: panel}
	}
	for _, c := range s.Nodes.Conns() {
		n := nodeAddrName{id: c.NodeID, name: s.nodeName(ctx, c.NodeID)}
		if ip, ok := c.Source(); ok && !ip.IsLoopback() {
			out[ip.Unmap()] = n
		}
		if r := c.LastReport(); r != nil && len(r.GetMesh().GetPublicKey()) == 32 {
			out[mesh.Address(mesh.Key(r.GetMesh().GetPublicKey()))] = n
		}
	}
	return out
}

type nodeAddrName struct{ id, name string }

// cleanConnections 填上对端节点的名称，并按成员的范围和权限清理（x 为 nil 的管理员只填名称）：
//   - 范围外项目的容器、面板不认识的容器（Docker 名称、不属于 Nagi 项目，和节点页一样只给管理员，C5）的连接整条去掉，
//     只计进 omitted：进程、端口、对端、流量都说明那个容器在做什么。
//   - Nagi 节点和面板的地址（对端，或者本端是节点自己的公网地址）要「查看访问地址」才给（B1、B2）；看不到的节点
//     一律不给地址，名称写 other（只隐藏名称的话，IP 照样认得出是哪台）。互联网上别的地址照常给：
//     查看节点详情本来就是看主机在连什么。
func cleanConnections(x *access, names map[netip.Addr]nodeAddrName, other string, snap *nagiv1.ConnectionSnapshot) {
	kept := snap.Connections[:0]
	for _, c := range snap.GetConnections() {
		if x != nil && (c.GetProjectId() != "" && !x.project(c.GetProjectId()) || c.GetProjectId() == "" && c.GetDockerName() != "") {
			snap.Omitted++
			continue
		}
		if n, ok := nagiAddr(names, c.GetRemoteIp()); ok {
			c.RemoteNodeName = n.name
			if n.id != "" && !x.node(n.id) {
				c.RemoteNodeName, c.RemoteIp = other, ""
			}
			if !x.can(auth.PermAddresses) {
				c.RemoteIp = ""
			}
		}
		if _, ok := nagiAddr(names, c.GetLocalIp()); ok && !x.can(auth.PermAddresses) {
			c.LocalIp = ""
		}
		kept = append(kept, c)
	}
	snap.Connections = kept
}

func nagiAddr(names map[netip.Addr]nodeAddrName, s string) (nodeAddrName, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return nodeAddrName{}, false
	}
	n, ok := names[ip.Unmap()]
	return n, ok
}
