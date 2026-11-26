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

// 安全审查 M1、M2（2d6a7f45）：
//   - 范围外项目的容器、不认识的容器（只给管理员，C5）的连接整条去掉并计进 omitted，不只是去掉名称：
//     进程、端口、对端、流量照样说明那个容器在做什么。
//   - Nagi 节点和面板的地址（对端，或本端是节点的公网地址）要「查看访问地址」；看不到的节点一律不给地址，
//     只换名称的话 IP 照样认得出是哪台。互联网上别的地址照常给。管理员（x 为 nil）看到全部。
func TestCleanConnections(t *testing.T) {
	names := map[netip.Addr]nodeAddrName{
		netip.MustParseAddr("203.0.113.5"):  {id: "n2", name: "HK-001"},
		netip.MustParseAddr("203.0.113.6"):  {id: "n3", name: "secret-node"},
		netip.MustParseAddr("198.51.100.1"): {name: "面板"},
		netip.MustParseAddr("203.0.113.1"):  {id: "n1", name: "self"},
	}
	mk := func() *nagiv1.ConnectionSnapshot {
		return &nagiv1.ConnectionSnapshot{Omitted: 1, Connections: []*nagiv1.NodeConnection{
			{LocalIp: "203.0.113.1", RemoteIp: "203.0.113.5", ProjectId: "p1", ProjectName: "博客", Container: "web", DockerName: "nagi-p1-web"},
			{LocalIp: "172.18.0.3", RemoteIp: "1.1.1.1", ProjectId: "p2", ProjectName: "别人的", Container: "db", Pid: 42, Process: "app", SendRate: 900},
			{LocalIp: "172.17.0.2", RemoteIp: "8.8.8.8", DockerName: "stray_container", Pid: 43, Process: "miner"},
			// IPv4 映射的 IPv6 地址也认得出是节点。
			{LocalIp: "203.0.113.1", RemoteIp: "::ffff:203.0.113.6", Pid: 7, Process: "nagi"},
			{LocalIp: "203.0.113.1", RemoteIp: "198.51.100.1", Pid: 7, Process: "nagi"},
			{LocalIp: "10.0.0.2", RemoteIp: "1.1.1.1", Pid: 8, Process: "curl"},
		}}
	}

	admin := mk()
	cleanConnections(nil, names, "其他节点", admin)
	if len(admin.Connections) != 6 || admin.Omitted != 1 {
		t.Fatalf("管理员看到全部：%v", admin)
	}
	if c := admin.Connections; c[0].GetRemoteNodeName() != "HK-001" || c[3].GetRemoteNodeName() != "secret-node" || c[3].GetRemoteIp() == "" ||
		c[4].GetRemoteNodeName() != "面板" || c[4].GetRemoteIp() != "198.51.100.1" || c[0].GetLocalIp() != "203.0.113.1" {
		t.Errorf("管理员：%v", c)
	}

	scoped := func(perms ...auth.Perm) *access {
		return &access{perms: auth.PermsOf(append([]auth.Perm{auth.PermNodeView, auth.PermNodeDetails}, perms...)...),
			projects: map[string]bool{"p1": true}, nodes: map[string]bool{"n1": true, "n2": true}}
	}

	m := mk()
	cleanConnections(scoped(), names, "其他节点", m)
	if m.Omitted != 3 || len(m.Connections) != 4 {
		t.Fatalf("范围外项目和不认识的容器整条去掉并计数：omitted=%d %v", m.Omitted, m.Connections)
	}
	for _, c := range m.Connections {
		if c.GetPid() == 42 || c.GetPid() == 43 || c.GetProjectId() == "p2" {
			t.Errorf("不该留下：%v", c)
		}
	}
	c := m.Connections
	if c[0].GetRemoteNodeName() != "HK-001" || c[0].GetRemoteIp() != "" || c[0].GetLocalIp() != "" || c[0].GetContainer() != "web" {
		t.Errorf("范围内的节点：名称照给，没有查看访问地址时两端的节点地址都不给：%v", c[0])
	}
	if c[1].GetRemoteNodeName() != "其他节点" || c[1].GetRemoteIp() != "" {
		t.Errorf("看不到的节点：%v", c[1])
	}
	if c[2].GetRemoteNodeName() != "面板" || c[2].GetRemoteIp() != "" {
		t.Errorf("面板：%v", c[2])
	}
	if c[3].GetRemoteIp() != "1.1.1.1" || c[3].GetLocalIp() != "10.0.0.2" {
		t.Errorf("互联网上的地址和内网地址照常给：%v", c[3])
	}

	a := mk()
	cleanConnections(scoped(auth.PermAddresses), names, "其他节点", a)
	c = a.Connections
	if c[0].GetRemoteIp() != "203.0.113.5" || c[0].GetLocalIp() != "203.0.113.1" || c[2].GetRemoteIp() != "198.51.100.1" {
		t.Errorf("有查看访问地址时给范围内节点和面板的地址：%v", c)
	}
	if c[1].GetRemoteNodeName() != "其他节点" || c[1].GetRemoteIp() != "" {
		t.Errorf("有查看访问地址也不给看不到的节点的地址：%v", c[1])
	}
}
