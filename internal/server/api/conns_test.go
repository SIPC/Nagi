package api

import (
	"context"
	"errors"
	"net/http/cookiejar"
	"net/netip"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 看连接要「查看节点详情」，只有查看节点的角色看不到；令牌只读也能看（不带命令行）。每次打开都记审计。
func TestWatchNodeConnectionsAccess(t *testing.T) {
	p := nagiv1connect.NodeServiceWatchNodeConnectionsProcedure
	if got := auth.ProcedureAccess(p); got != "node.details" {
		t.Fatalf("登记为 %q，应为 node.details", got)
	}
	if auth.Allowed(auth.MemberWith(9, auth.PresetViewer), p) {
		t.Error("只读角色（没有查看节点详情）不应能看网络连接")
	}
	if !auth.Allowed(auth.MemberWith(9, auth.PermsOf(auth.PermNodeView, auth.PermNodeDetails)), p) {
		t.Error("有查看节点详情的成员应能看")
	}
	if needsElevation(auth.User{ID: 1, Role: auth.RoleAdmin}, p) {
		t.Error("不带命令行时不要高权限确认")
	}
	if !activityRecorded(p) || categoryOf(methodOf(p)) != "read" {
		t.Errorf("每次打开都要记审计（敏感查询）：recorded=%v category=%q", activityRecorded(p), categoryOf(methodOf(p)))
	}
	if !store.SensitiveReads["WatchNodeConnections"] {
		t.Error("应登记在 SensitiveReads")
	}
}

// 要命令行：没有「节点终端和文件」拒绝；有权限但没做高权限确认时要求确认（带标记，界面弹框）。在连节点之前就拒绝。
func TestWatchNodeConnectionsCmdline(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	if _, err := h.a.Auth.CreateUser(ctx, "admin", "admin-password-1", auth.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(ctx, connect.NewRequest(&nagiv1.LoginRequest{Username: "admin", Password: "admin-password-1"})); err != nil {
		t.Fatal(err)
	}
	nodes := nagiv1connect.NewNodeServiceClient(&hc, h.srv.URL)
	stream, err := nodes.WatchNodeConnections(ctx, connect.NewRequest(&nagiv1.WatchNodeConnectionsRequest{NodeId: "n1", Cmdline: true}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get(elevationHeader) != "required" {
		t.Fatalf("没有高权限确认时应要求确认：%v", err)
	}

	// 访问令牌做不了确认，不能要命令行。
	token := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 3, AllowWrite: true, AllowNodeExec: true}
	if token.ElevatedFor(nagiv1connect.NodeServiceWatchNodeConnectionsProcedure) {
		t.Error("令牌不能算做过确认")
	}
	// 没有「节点终端和文件」的成员：权限检查在确认之前，直接拒绝。
	member := auth.MemberWith(9, auth.PermsOf(auth.PermNodeView, auth.PermNodeDetails))
	member.ElevatedUntil = 1 << 40
	if err := h.a.nodeRootAllowed(ctx, member, "n1"); err == nil {
		t.Error("没有节点终端和文件不能看命令行")
	}
}

// 对端是 Nagi 节点时填上名称，看不到的节点是「其他节点」；范围外项目的容器不给项目、容器名和 Docker 名称。
// 管理员（x 为 nil）看到全部。
func TestCleanConnections(t *testing.T) {
	names := map[netip.Addr]nodeAddrName{
		netip.MustParseAddr("203.0.113.5"): {id: "n2", name: "HK-001"},
		netip.MustParseAddr("203.0.113.6"): {id: "n3", name: "secret-node"},
	}
	mk := func() *nagiv1.ConnectionSnapshot {
		return &nagiv1.ConnectionSnapshot{Connections: []*nagiv1.NodeConnection{
			{RemoteIp: "203.0.113.5", ProjectId: "p1", ProjectName: "博客", Container: "web", DockerName: "nagi-p1-web"},
			{RemoteIp: "203.0.113.6", ProjectId: "p2", ProjectName: "别人的", Container: "db", Service: "db", DockerName: "nagi-p2-db"},
			{RemoteIp: "1.1.1.1", Process: "curl"},
		}}
	}
	admin := mk()
	cleanConnections(nil, names, "其他节点", admin)
	if c := admin.Connections; c[0].GetRemoteNodeName() != "HK-001" || c[1].GetRemoteNodeName() != "secret-node" || c[1].GetContainer() != "db" || c[2].GetRemoteNodeName() != "" {
		t.Errorf("管理员：%v", c)
	}

	x := &access{perms: auth.PermsOf(auth.PermNodeView, auth.PermNodeDetails), projects: map[string]bool{"p1": true}, nodes: map[string]bool{"n1": true, "n2": true}}
	m := mk()
	cleanConnections(x, names, "其他节点", m)
	c := m.Connections
	if c[0].GetRemoteNodeName() != "HK-001" || c[0].GetContainer() != "web" || c[0].GetDockerName() != "nagi-p1-web" {
		t.Errorf("范围内：%v", c[0])
	}
	if c[1].GetRemoteNodeName() != "其他节点" || c[1].GetProjectId() != "" || c[1].GetProjectName() != "" || c[1].GetContainer() != "" || c[1].GetService() != "" || c[1].GetDockerName() != "" {
		t.Errorf("范围外：%v", c[1])
	}
	if c[2].GetProcess() != "curl" {
		t.Errorf("不是容器的进程照常显示：%v", c[2])
	}
}
