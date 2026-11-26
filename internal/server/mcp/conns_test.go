package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 看连接和节点页一样要「查看节点详情」：只读令牌也行（不带命令行），没有这项权限的角色看不到工具。
func TestListNodeConnectionsAllowed(t *testing.T) {
	var tl tool
	for _, x := range allTools {
		if x.name == "list_node_connections" {
			tl = x
		}
	}
	if tl.add == nil || tl.write || tl.exec || tl.nodeExec {
		t.Fatalf("应是只读工具：%+v", tl)
	}
	details := auth.MemberWith(9, auth.PermsOf(auth.PermNodeView, auth.PermNodeDetails))
	details.TokenID = 1
	viewer := auth.MemberWith(9, auth.PresetViewer)
	viewer.TokenID = 1
	if !tl.allowedFor(details, false) || !tl.allowedFor(details, true) {
		t.Error("有查看节点详情的只读令牌（包括备用面板上）应能用")
	}
	if tl.allowedFor(viewer, false) {
		t.Error("没有查看节点详情的角色不应看到这个工具")
	}
}

// fakeConns 是面板的 WatchNodeConnections：按顺序发 snaps，然后按 err 结束（nil 时一直等到断开）。
type fakeConns struct {
	nagiv1connect.UnimplementedNodeServiceHandler
	snaps []*nagiv1.ConnectionSnapshot
	err   error
	got   *nagiv1.WatchNodeConnectionsRequest
}

func (f *fakeConns) WatchNodeConnections(ctx context.Context, req *connect.Request[nagiv1.WatchNodeConnectionsRequest], out *connect.ServerStream[nagiv1.ConnectionSnapshot]) error {
	f.got = req.Msg
	for _, s := range f.snaps {
		if err := out.Send(s); err != nil {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	<-ctx.Done()
	return nil
}

func connsClient(t *testing.T, f *fakeConns) nagiv1connect.NodeServiceClient {
	mux := http.NewServeMux()
	mux.Handle(nagiv1connect.NewNodeServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return nagiv1connect.NewNodeServiceClient(srv.Client(), srv.URL)
}

// 取第二轮（有速率）就返回，不要命令行；节点的错误（旧节点、看的人满了）原样返回。
func TestSampleConnections(t *testing.T) {
	first := &nagiv1.ConnectionSnapshot{Connections: []*nagiv1.NodeConnection{{RemoteIp: "1.1.1.1"}}}
	second := &nagiv1.ConnectionSnapshot{Connections: []*nagiv1.NodeConnection{{RemoteIp: "1.1.1.1", SendRate: 10}}}
	f := &fakeConns{snaps: []*nagiv1.ConnectionSnapshot{first, second}}
	snap, rated, err := sampleConnections(context.Background(), connsClient(t, f), "n1")
	if err != nil || !rated || snap.GetConnections()[0].GetSendRate() != 10 {
		t.Fatalf("应返回第二轮：%v %v %v", snap, rated, err)
	}
	if f.got.GetNodeId() != "n1" || f.got.GetCmdline() {
		t.Errorf("请求：%v（不能要命令行）", f.got)
	}

	old := &fakeConns{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("节点版本过低，升级后才能使用此功能"))}
	if _, _, err := sampleConnections(context.Background(), connsClient(t, old), "n1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("旧节点的错误应原样返回：%v", err)
	}
	// 发了一轮就报错（不是超时）：报错，不拿这一轮冒充结果。
	broken := &fakeConns{snaps: []*nagiv1.ConnectionSnapshot{first}, err: connect.NewError(connect.CodeUnavailable, errors.New("node went offline"))}
	if _, _, err := sampleConnections(context.Background(), connsClient(t, broken), "n1"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("中途断开应报错：%v", err)
	}
}

// 默认只看出站，筛选匹配远端、节点名、进程、容器；按速率排序截到 limit；分组按速率合计排序，内核的连接记成 kernel。
func TestConnsView(t *testing.T) {
	snap := &nagiv1.ConnectionSnapshot{IntervalMs: 2000, Omitted: 3, Connections: []*nagiv1.NodeConnection{
		{Protocol: "tcp", RemoteIp: "192.168.0.13", RemotePort: 3260, LocalIp: "192.168.10.239", LocalPort: 51000, SendRate: 900, BytesSent: 5 << 30},
		{Protocol: "tcp", RemoteIp: "203.0.113.5", RemotePort: 7002, RemoteNodeName: "HK-001", Process: "nagi", Pid: 10, Unit: "nagi-agent.service", SendRate: 50},
		{Protocol: "tcp", RemoteIp: "1.1.1.1", RemotePort: 443, Process: "curl", Pid: 20, ProjectName: "博客", Container: "web", Service: "app", SendRate: 100, ReceiveRate: 300},
		{Protocol: "tcp", RemoteIp: "1.1.1.1", RemotePort: 80, Process: "curl", Pid: 21, ProjectName: "博客", Container: "web", Service: "app", ReceiveRate: 5},
		{Protocol: "tcp", RemoteIp: "10.0.0.9", RemotePort: 50123, LocalPort: 443, Inbound: true, Process: "caddy", SendRate: 10000},
		{Protocol: "udp", RemoteIp: "2001:db8::1", RemotePort: 53, Process: "resolved"},
	}}

	out := connsView(snap, "out", "", "", 50)
	if out.Matched != 5 || len(out.Connections) != 5 {
		t.Fatalf("出站应有 5 条（入站的 caddy 不算）：%+v", out)
	}
	if c := out.Connections[0]; c.Remote != "192.168.0.13:3260" || c.Process != "kernel" || c.PID != 0 || c.SendBPS != 900 {
		t.Errorf("最快的是内核的 iSCSI：%+v", c)
	}
	if out.Connections[1].Remote != "1.1.1.1:443" || out.Connections[4].Remote != "[2001:db8::1]:53" {
		t.Errorf("按收发合计排序，IPv6 带方括号：%+v", out.Connections)
	}
	if out.NodeOmitted != 3 || out.SampleSeconds != 2 || len(out.Notes) != 1 {
		t.Errorf("省略数、间隔和命令行说明：%+v", out)
	}

	if in := connsView(snap, "in", "", "", 50); in.Matched != 1 || in.Connections[0].Direction != "in" {
		t.Errorf("只看入站：%+v", in)
	}
	if all := connsView(snap, "all", "", "", 50); all.Matched != 6 {
		t.Errorf("全部：%d", all.Matched)
	}
	for filter, want := range map[string]int{"hk-001": 1, "1.1.1.1:80": 1, "博客": 2, "agent.service": 1, "3260": 1, "nothing": 0} {
		if got := connsView(snap, "out", filter, "", 50).Matched; got != want {
			t.Errorf("筛选 %q：%d 条，想要 %d", filter, got, want)
		}
	}

	limited := connsView(snap, "out", "", "", 2)
	if limited.Matched != 5 || len(limited.Connections) != 2 || len(limited.Notes) != 2 {
		t.Errorf("截到 2 条并提示还有 3 条：%+v", limited)
	}

	g := connsView(snap, "out", "", "process", 1).Groups
	if len(g) != 4 || g[0].Name != "kernel" || g[1].Name != "curl" || g[1].Connections != 2 || g[1].SendBPS != 100 || g[1].RecvBPS != 305 {
		t.Errorf("按进程分组（分组不受 limit 影响）：%+v", g)
	}
	g = connsView(snap, "out", "", "container", 50).Groups
	if g[1].Name != "博客 / web / app" || g[2].Name != "host nagi-agent.service" {
		t.Errorf("按容器分组：%+v", g)
	}
	g = connsView(snap, "out", "", "remote", 50).Groups
	if g[1].Name != "1.1.1.1" || g[1].Connections != 2 || g[2].Name != "203.0.113.5 (HK-001)" {
		t.Errorf("按远端 IP 分组：%+v", g)
	}
}
