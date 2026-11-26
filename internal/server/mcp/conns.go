package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 节点的网络连接（见 docs/mcp.md#网络连接、docs/agent.md#网络连接）：和节点页的「网络连接」用同一个接口
// WatchNodeConnections，权限（「查看节点详情」）、范围清理（看不到的节点、范围外项目的容器）和审计都在接口里做，
// 这次工具调用下面挂一条 WatchNodeConnections。取两轮采样（第二轮才有速率）就断开，不跟随，不写盘。
// 命令行要「节点终端和文件」并做过高权限确认，令牌做不了确认（auth.User.ElevatedFor），所以不要命令行，结果里说明原因。

func init() {
	allTools = append(allTools, def(tool{name: "list_node_connections", title: "List node connections", idempotent: true,
		description: "Live network connections on a node, as in the node page's Connections tab: protocol, state, direction, local and remote address, the Nagi node at the remote end, the process (PID, name, systemd unit) and container (project / container / service) holding each one, and send/receive rates in bytes per second. " +
			"Takes two samples 2 to 5 seconds apart (rates come from the second) and returns; nothing is stored. Connections that open and close between samples are not seen. " +
			"Process command lines are not returned: they may contain passwords or tokens and need the node terminal permission plus re-authentication, which an access token cannot do; look at them in the panel. " +
			"Needs the View node details permission. Returns at most 200 connections, busiest first, optionally summed up by process, container or remote.",
		procedures: []string{nagiv1connect.NodeServiceListNodesProcedure, nagiv1connect.NodeServiceWatchNodeConnectionsProcedure}},
		toolListNodeConnections))
}

const (
	// connsSamples：第一轮只有累计字节，第二轮才有速率。
	connsSamples = 2
	// connsWait 是等采样的最长时间：节点忙时一轮间隔 5 秒。
	connsWait = 15 * time.Second
	// connsDefaultLimit、connsMaxLimit 是返回的连接数；分组最多 connsMaxGroups 个。
	connsDefaultLimit = 50
	connsMaxLimit     = 200
	connsMaxGroups    = 50
)

// cmdlineNote 是不返回命令行的原因（给 agent 转告用户）。
const cmdlineNote = "Process command lines are not included: they can contain passwords or tokens, so the panel only shows them to members with the node terminal permission after re-authentication, which an access token cannot do. Open the node's Connections tab in the panel to see them."

type NodeConnectionsArgs struct {
	Node      string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Direction string `json:"direction,omitempty" jsonschema:"out (default): connections this node opened; in: connections to ports it listens on; all"`
	Filter    string `json:"filter,omitempty" jsonschema:"only connections whose remote address (IP or IP:port), remote node, process, unit, project or container contains this text"`
	GroupBy   string `json:"group_by,omitempty" jsonschema:"also sum up the matching connections by process, container or remote (remote IP). Default no groups"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum connections to return, busiest first. Default 50, at most 200"`
}

type connView struct {
	Protocol   string `json:"protocol"`
	State      string `json:"state,omitempty"`
	Direction  string `json:"direction"`
	Local      string `json:"local"`
	Remote     string `json:"remote"`
	RemoteNode string `json:"remote_node,omitempty"`
	// Process 为空、PID 为 0：内核自己的连接（iSCSI、NFS）或已经关掉的（TIME_WAIT）。
	Process    string  `json:"process"`
	PID        uint32  `json:"pid,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Project    string  `json:"project,omitempty"`
	Container  string  `json:"container,omitempty"`
	Service    string  `json:"service,omitempty"`
	DockerName string  `json:"docker_name,omitempty"`
	SendBPS    float64 `json:"send_bytes_per_sec"`
	RecvBPS    float64 `json:"receive_bytes_per_sec"`
	BytesSent  uint64  `json:"bytes_sent,omitempty"`
	BytesRecv  uint64  `json:"bytes_received,omitempty"`
	RTTMs      float64 `json:"rtt_ms,omitempty"`
}

type connGroup struct {
	Name        string  `json:"name"`
	Connections int     `json:"connections"`
	SendBPS     float64 `json:"send_bytes_per_sec"`
	RecvBPS     float64 `json:"receive_bytes_per_sec"`
}

type connsResult struct {
	Node string `json:"node"`
	// Matched 是符合条件的连接数，Connections 只列前 limit 条。
	Matched     int         `json:"matched"`
	Connections []connView  `json:"connections"`
	Groups      []connGroup `json:"groups,omitempty"`
	// NodeOmitted 是节点一轮超过 2000 条、没有发过来的连接数（不在 matched 里）。
	NodeOmitted     uint32   `json:"node_omitted,omitempty"`
	SampleSeconds   float64  `json:"sample_interval_seconds"`
	NamespaceErrors uint32   `json:"namespace_errors,omitempty"`
	Notes           []string `json:"notes"`
}

func toolListNodeConnections(c *call, in NodeConnectionsArgs) (any, error) {
	dir := cmp.Or(in.Direction, "out")
	if dir != "out" && dir != "in" && dir != "all" {
		return nil, fmt.Errorf("direction must be out, in or all, not %q", in.Direction)
	}
	if in.GroupBy != "" && in.GroupBy != "process" && in.GroupBy != "container" && in.GroupBy != "remote" {
		return nil, fmt.Errorf("group_by must be process, container or remote, not %q", in.GroupBy)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = connsDefaultLimit
	}
	limit = min(limit, connsMaxLimit)
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	snap, rated, err := sampleConnections(c.ctx, c.h.clients.nodes, n.GetId())
	if err != nil {
		return nil, err
	}
	out := connsView(snap, dir, in.Filter, in.GroupBy, limit)
	out.Node = n.GetName()
	if !rated {
		out.Notes = append(out.Notes, "Only one sample arrived in time, so rates are 0; call again for rates.")
	}
	return out, nil
}

// sampleConnections 取 connsSamples 轮采样，返回最后一轮；rated 表示有速率（取到了第二轮）。返回时断开，节点没人看就停止采样。
func sampleConnections(ctx context.Context, nodes nagiv1connect.NodeServiceClient, nodeID string) (snap *nagiv1.ConnectionSnapshot, rated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, connsWait)
	defer cancel()
	// 不要命令行：令牌做不了高权限确认（见上）。
	stream, err := nodes.WatchNodeConnections(ctx, connect.NewRequest(&nagiv1.WatchNodeConnectionsRequest{NodeId: nodeID}))
	if err != nil {
		return nil, false, err
	}
	defer stream.Close()
	got := 0
	for got < connsSamples && stream.Receive() {
		snap = stream.Msg()
		got++
	}
	if got == connsSamples {
		return snap, true, nil
	}
	// 等到超时：有一轮就用它（没有速率），一轮都没有时报超时。其余的错误（没有权限、旧节点、看的人满了）原样返回。
	if ctx.Err() == nil {
		if err := stream.Err(); err != nil {
			return nil, false, err
		}
	}
	if snap == nil {
		return nil, false, errors.New("the node sent no sample in time, try again")
	}
	return snap, false, nil
}

// connsView 按方向和文字筛选，按速率排序截到 limit 条，需要时分组。
func connsView(snap *nagiv1.ConnectionSnapshot, dir, filter, groupBy string, limit int) connsResult {
	filter = strings.ToLower(strings.TrimSpace(filter))
	var matched []*nagiv1.NodeConnection
	for _, x := range snap.GetConnections() {
		if dir == "out" && x.GetInbound() || dir == "in" && !x.GetInbound() {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(strings.Join([]string{hostPort(x.GetRemoteIp(), x.GetRemotePort()), x.GetRemoteNodeName(),
			x.GetProcess(), x.GetUnit(), x.GetProjectName(), x.GetContainer(), x.GetService(), x.GetDockerName()}, "\x00")), filter) {
			continue
		}
		matched = append(matched, x)
	}
	slices.SortStableFunc(matched, func(a, b *nagiv1.NodeConnection) int {
		if c := cmp.Compare(b.GetSendRate()+b.GetReceiveRate(), a.GetSendRate()+a.GetReceiveRate()); c != 0 {
			return c
		}
		return cmp.Compare(b.GetBytesSent()+b.GetBytesReceived(), a.GetBytesSent()+a.GetBytesReceived())
	})
	out := connsResult{Matched: len(matched), Connections: []connView{}, NodeOmitted: snap.GetOmitted(),
		SampleSeconds: float64(snap.GetIntervalMs()) / 1000, NamespaceErrors: snap.GetNamespaceErrors(), Notes: []string{cmdlineNote}}
	for _, x := range matched[:min(limit, len(matched))] {
		v := connView{Protocol: x.GetProtocol(), State: x.GetState(), Direction: "out",
			Local: hostPort(x.GetLocalIp(), x.GetLocalPort()), Remote: hostPort(x.GetRemoteIp(), x.GetRemotePort()), RemoteNode: x.GetRemoteNodeName(),
			Process: cmp.Or(x.GetProcess(), "kernel"), PID: x.GetPid(), Unit: x.GetUnit(),
			Project: x.GetProjectName(), Container: x.GetContainer(), Service: x.GetService(), DockerName: x.GetDockerName(),
			SendBPS: math.Round(x.GetSendRate()), RecvBPS: math.Round(x.GetReceiveRate()), BytesSent: x.GetBytesSent(), BytesRecv: x.GetBytesReceived(),
			RTTMs: math.Round(float64(x.GetRttUs())/100) / 10}
		if x.GetInbound() {
			v.Direction = "in"
		}
		out.Connections = append(out.Connections, v)
	}
	if groupBy != "" {
		out.Groups = connGroups(matched, groupBy)
	}
	if len(matched) > limit {
		out.Notes = append(out.Notes, fmt.Sprintf("%d more matching connections are not listed; narrow with filter or raise limit (at most %d).", len(matched)-limit, connsMaxLimit))
	}
	return out
}

// connGroups 把连接按进程、容器或远端 IP 加起来，按速率合计排序，最多 connsMaxGroups 组。
func connGroups(list []*nagiv1.NodeConnection, by string) []connGroup {
	idx := map[string]int{}
	var groups []connGroup
	for _, x := range list {
		var key string
		switch by {
		case "process":
			key = cmp.Or(x.GetProcess(), "kernel")
		case "container":
			switch {
			case x.GetContainer() != "":
				key = x.GetProjectName() + " / " + x.GetContainer()
				if s := x.GetService(); s != "" && s != x.GetContainer() {
					key += " / " + s
				}
			case x.GetDockerName() != "":
				key = x.GetDockerName()
			default:
				key = "host " + cmp.Or(x.GetUnit(), "(no unit)")
			}
		case "remote":
			key = x.GetRemoteIp()
			if n := x.GetRemoteNodeName(); n != "" {
				key += " (" + n + ")"
			}
		}
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, connGroup{Name: key})
		}
		g := &groups[i]
		g.Connections++
		g.SendBPS += x.GetSendRate()
		g.RecvBPS += x.GetReceiveRate()
	}
	slices.SortStableFunc(groups, func(a, b connGroup) int { return cmp.Compare(b.SendBPS+b.RecvBPS, a.SendBPS+a.RecvBPS) })
	groups = groups[:min(len(groups), connsMaxGroups)]
	for i := range groups {
		groups[i].SendBPS, groups[i].RecvBPS = math.Round(groups[i].SendBPS), math.Round(groups[i].RecvBPS)
	}
	return groups
}

func hostPort(ip string, port uint32) string {
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, strconv.FormatUint(uint64(port), 10))
}
