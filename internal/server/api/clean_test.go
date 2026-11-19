package api

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/nodes"
	"google.golang.org/protobuf/proto"
	"slices"
	"sort"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// addressTokens 是字段名里表示地址、节点引用、节点上路径的词。回复里名字带这些词的字段都要登记在 cleanedFields 里，
// 写明怎么处理（清空、换成「其他节点」、要哪项权限、保留的理由）。
var addressTokens = map[string]bool{
	"node": true, "nodes": true, "address": true, "addresses": true, "addr": true, "ip": true, "ips": true,
	"endpoint": true, "endpoints": true, "host": true, "hostname": true, "mount": true, "mountpoint": true,
	"fingerprint": true, "holder": true, "via": true, "dir": true, "dirs": true, "witness": true, "root": true,
}

// TestCleanedFieldsRegistered：非管理员能调用的接口（ProjectService、NodeService、SettingsService、AppService）的回复里，
// 所有带地址、节点引用、路径的字段都要登记处理方式。新增字段忘了处理时这里失败。
func TestCleanedFieldsRegistered(t *testing.T) {
	member := auth.MemberWith(9, auth.AllPerms)
	seen := map[protoreflect.FullName]bool{}
	var missing []string
	found := map[string]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] || md.ParentFile().Package() != "nagi.v1" || md.IsMapEntry() {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			key := string(md.FullName()) + "." + string(f.Name())
			for _, tok := range strings.Split(string(f.Name()), "_") {
				if addressTokens[tok] {
					found[key] = true
					if cleanedFields[key] == "" {
						missing = append(missing, key)
					}
					break
				}
			}
			if f.Message() != nil {
				walk(f.Message())
			}
			if f.IsMap() && f.MapValue().Message() != nil {
				walk(f.MapValue().Message())
			}
		}
	}
	services := nagiv1.File_nagi_v1_api_proto.Services()
	for i := range services.Len() {
		svc := services.Get(i)
		switch svc.Name() {
		case "ProjectService", "NodeService", "SettingsService", "AppService":
		default:
			continue
		}
		for j := range svc.Methods().Len() {
			m := svc.Methods().Get(j)
			p := "/nagi.v1." + string(svc.Name()) + "/" + string(m.Name())
			if !auth.Allowed(member, p) {
				continue // 只给管理员的接口
			}
			walk(m.Output())
		}
	}
	sort.Strings(missing)
	for _, k := range slices.Compact(missing) {
		t.Errorf("回复字段 %s 带地址或节点引用，没有登记在 cleanedFields（clean.go）", k)
	}
	// 登记的字段都存在（改名、删字段时一起改）。
	for k := range cleanedFields {
		i := strings.LastIndexByte(k, '.')
		md, err := protoregistryFind(k[:i])
		if err != nil || md.Fields().ByName(protoreflect.Name(k[i+1:])) == nil {
			t.Errorf("cleanedFields 里的 %s 不存在", k)
		}
	}
}

// cleanTest 搭一个有两台节点（n1 在范围内、n2 在范围外）的环境。
func cleanTest(t *testing.T) *API {
	a, _ := scopeTestAPI(t)
	return a
}

func memberAccess(perms auth.Perms) *access {
	return &access{perms: perms.Normalize(), projects: map[string]bool{"p1": true}, nodes: map[string]bool{"n1": true}}
}

func richProject() *nagiv1.Project {
	return &nagiv1.Project{
		Id: "p1",
		Routes: []*nagiv1.Exposure{{Name: "web-80", Container: "web", Published: 8080, Domains: []string{"a.example.com"},
			EntryNodes: []string{"n1", "n2"}}},
		Links: []*nagiv1.LinkedProject{{ProjectId: "p2", Name: "two", NetworkName: "two"}},
		Settings: &nagiv1.ProjectSettings{SharedEnv: []*nagiv1.EnvVar{{Key: "K", Value: "v"}}, InternalPorts: []string{"web:80"},
			AlertRules: &nagiv1.AlertRules{}, Egress: &nagiv1.Egress{Mode: "off"}, SnapshotLimit: 3},
		Containers: []*nagiv1.Container{{
			Name: "web", NodeId: "n2", Dir: "/srv/web", Config: &nagiv1.ContainerConfig{Compose: "x"}, DeployKey: "k", WebhookToken: "w",
			BackupNodeId: "n2", BackupNodeIds: []string{"n2", "n1"},
			Backups:           []*nagiv1.BackupStatus{{TargetNodeId: "n2", TargetName: "b", LastError: "connect to b failed"}, {TargetNodeId: "n1", TargetName: "a"}},
			BackupReplicaDirs: map[string]string{"n2": "/data/r", "n1": "/data/s"},
			Failover: &nagiv1.FailoverConfig{A: &nagiv1.FailoverParticipant{NodeId: "n2", Name: "b", Key: []byte{2}, Dir: "/srv/web"},
				B: &nagiv1.FailoverParticipant{NodeId: "n1", Name: "a", Key: []byte{1}, Dir: "/srv/web"}},
			FailoverHolder: &nagiv1.FailoverStatus{HolderName: "b", HolderKey: []byte{2}},
			AlertRules:     &nagiv1.AlertRules{}, SnapshotSchedule: &nagiv1.SnapshotSchedule{IntervalMinutes: 60},
			PendingHa: &nagiv1.PendingHA{BackupNodeIds: []string{"n2"}, Failover: &nagiv1.FailoverConfig{}},
		}},
	}
}

// 每项权限给和不给：回复里相应的字段去掉；范围外的节点 n2 一律看不到（名称、ID、地址、目录、公钥）。
func TestCleanProjectByPermission(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	all := auth.AllPerms.Normalize()
	cases := []struct {
		name  string
		perms auth.Perms
		check func(p *nagiv1.Project) bool
	}{
		{"没有查看配置：去掉配置、变量、目录、密钥", all &^ auth.PermsOf(auth.PermConfig, auth.PermDeploy, auth.PermPrivileged), func(p *nagiv1.Project) bool {
			c := p.Containers[0]
			return c.Config == nil && c.DeployKey == "" && c.WebhookToken == "" && c.Dir == "" && p.Settings.SharedEnv == nil
		}},
		{"没有备份：去掉备份节点和同步状态", all &^ auth.PermsOf(auth.PermBackup), func(p *nagiv1.Project) bool {
			c := p.Containers[0]
			return c.BackupNodeIds == nil && c.Backups == nil && c.BackupReplicaDirs == nil && c.PendingHa.GetBackupNodeIds() == nil
		}},
		{"没有高可用：去掉顶替", all &^ auth.PermsOf(auth.PermHA), func(p *nagiv1.Project) bool {
			c := p.Containers[0]
			return c.Failover == nil && c.FailoverHolder == nil && c.PendingHa.GetFailover() == nil
		}},
		{"没有网络和域名：去掉端口映射、连接、内部端口、出站", all &^ auth.PermsOf(auth.PermNetwork, auth.PermAddresses), func(p *nagiv1.Project) bool {
			return p.Routes == nil && p.Links == nil && p.Settings.InternalPorts == nil && p.Settings.Egress == nil
		}},
		{"没有告警规则", all &^ auth.PermsOf(auth.PermAlerts), func(p *nagiv1.Project) bool {
			return p.Settings.AlertRules == nil && p.Containers[0].AlertRules == nil
		}},
		{"没有快照", all &^ auth.PermsOf(auth.PermSnapshots), func(p *nagiv1.Project) bool {
			return p.Settings.SnapshotLimit == 0 && p.Containers[0].SnapshotSchedule == nil
		}},
		{"全部权限：范围外的节点是其他节点", all, func(p *nagiv1.Project) bool {
			c := p.Containers[0]
			return c.NodeId == "" && c.Dir == "" && c.BackupNodeId == "" && slices.Equal(c.BackupNodeIds, []string{"", "n1"}) &&
				c.Backups[0].TargetNodeId == "" && c.Backups[0].TargetName == "" && c.Backups[1].TargetName == "a" &&
				c.Backups[0].LastError == "connect to 其他节点 failed" &&
				c.BackupReplicaDirs["n2"] == "" && c.BackupReplicaDirs["n1"] == "/data/s" &&
				c.Failover.A.NodeId == "" && c.Failover.A.Name == "" && c.Failover.A.Key == nil && c.Failover.A.Dir == "" &&
				c.Failover.B.Name == "a" && c.FailoverHolder.HolderName == "" && c.FailoverHolder.HolderKey == nil &&
				slices.Equal(p.Routes[0].EntryNodes, []string{"n1", ""}) && p.Links[0].Name == "" && p.Links[0].NetworkName == "two" &&
				c.Config != nil
		}},
		{"没有查看节点：所有节点都是其他节点", all &^ auth.PermsOf(auth.PermNodeView, auth.PermNodeDetails, auth.PermNodeManage, auth.PermNodeImages, auth.PermNodeRoot), func(p *nagiv1.Project) bool {
			c := p.Containers[0]
			return slices.Equal(c.BackupNodeIds, []string{"", ""}) && c.Failover.B.Name == "" && c.Backups[1].TargetName == ""
		}},
	}
	for _, c := range cases {
		p := richProject()
		a.filterReply(ctx, memberAccess(c.perms), p)
		if !c.check(p) {
			t.Errorf("%s：%v", c.name, p)
		}
	}
	// 管理员不清理。
	p := richProject()
	a.filterReply(ctx, nil, p)
	if p.Containers[0].NodeId != "n2" || p.Containers[0].Config == nil {
		t.Error("管理员的回复不应清理")
	}
}

// 节点列表：范围外的节点整条去掉，other_nodes 不再返回；地址只给管理员（B1）；详情要「查看节点详情」（C1）；
// 不认识的容器、旧项目只给管理员（C5）。
func TestCleanNodes(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	res := func() *nagiv1.ListNodesResponse {
		return &nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{
			{Id: "n1", Name: "a", Address: "203.0.113.1", Online: true, AlertRules: &nagiv1.AlertRules{},
				UnknownContainers: []*nagiv1.UnknownContainer{{ProjectName: "x"}}, StaleProjects: []*nagiv1.StaleProject{{ProjectName: "y"}},
				Info:  &nagiv1.NodeInfo{Hostname: "host-a", Os: "linux", Version: "0.34", ProjectsRoot: "/srv", ReplicaDir: "/r", DirectDownload: &nagiv1.DirectDownload{Address: "203.0.113.1"}, Cpus: 4},
				Usage: &nagiv1.NodeUsage{Disks: []*nagiv1.DiskUsage{{Mount: "/", TotalBytes: 10}}}},
			{Id: "n2", Name: "b", Address: "203.0.113.2", Online: true},
		}}
	}
	m := res()
	a.filterReply(ctx, memberAccess(auth.PresetOperator), m)
	if len(m.Nodes) != 1 || len(m.OtherNodes) != 0 {
		t.Fatalf("只留范围内的节点：%v", m)
	}
	n := m.Nodes[0]
	if n.Address != "" || n.Info.DirectDownload.Address != "" || n.UnknownContainers != nil || n.StaleProjects != nil || n.AlertRules != nil {
		t.Errorf("地址、不认识的容器、告警规则应去掉：%v", n)
	}
	if n.Info.Hostname != "" || n.Info.Os != "" || n.Info.Version != "" || n.Info.ProjectsRoot != "" || n.Info.ReplicaDir != "" || n.Usage.Disks[0].Mount != "" {
		t.Errorf("没有查看节点详情时去掉详情：%v", n.Info)
	}
	if n.Info.Cpus != 4 || n.Usage.Disks[0].TotalBytes != 10 || !n.Online {
		t.Error("整体的占用和配置保留")
	}
	m = res()
	a.filterReply(ctx, memberAccess(auth.PresetOperator|auth.PermsOf(auth.PermNodeDetails, auth.PermNodeManage)), m)
	if n := m.Nodes[0]; n.Info.Hostname != "host-a" || n.AlertRules == nil || n.Address != "" {
		t.Errorf("有查看节点详情、节点管理：%v", n)
	}
	// 有「查看访问地址」时给看得到的节点的地址（端口映射的 IP:端口），看不到的节点仍然整条去掉。
	m = res()
	a.filterReply(ctx, memberAccess(auth.PresetOperator|auth.PermsOf(auth.PermAddresses)), m)
	if len(m.Nodes) != 1 || m.Nodes[0].Address != "203.0.113.1" || m.Nodes[0].Info.DirectDownload.Address == "" {
		t.Errorf("有查看访问地址：%v", m.Nodes)
	}
	m = res()
	a.filterReply(ctx, memberAccess(auth.PermsOf(auth.PermView)), m)
	if len(m.Nodes) != 0 {
		t.Error("没有查看节点时看不到任何节点")
	}
}

// 域名检查（A2、B7、C7）：范围外的入口只给结论；地址要「查看访问地址」；证书失败原因去掉 IP。
func TestCleanDomains(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	res := func() *nagiv1.CheckDomainsResponse {
		return &nagiv1.CheckDomainsResponse{Domains: []*nagiv1.DomainCheck{{Domain: "a.example.com", Addresses: []string{"203.0.113.9"},
			Entries: []*nagiv1.DomainEntryCheck{
				{NodeId: "n1", NodeName: "a", Address: "203.0.113.1", Dns: "mismatch", Cert: "failed", CertError: "dial 203.0.113.9:443: refused"},
				{NodeId: "n2", NodeName: "b", Address: "203.0.113.2", Dns: "ok", Cert: "obtained"},
			}}}}
	}
	m := res()
	a.filterReply(ctx, memberAccess(auth.PresetOperator|auth.PermsOf(auth.PermNetwork)), m)
	d := m.Domains[0]
	if d.Addresses != nil || d.Entries[0].Address != "" || strings.Contains(d.Entries[0].CertError, "203.0.113") || d.Entries[0].NodeName != "a" {
		t.Errorf("没有查看访问地址：%v", d)
	}
	if e := d.Entries[1]; e.NodeId != "" || e.NodeName != "" || e.Address != "" || e.Dns != "ok" || e.Cert != "obtained" {
		t.Errorf("范围外的入口只给结论：%v", e)
	}
	m = res()
	a.filterReply(ctx, memberAccess(auth.PresetOperator|auth.PermsOf(auth.PermAddresses)), m)
	if d := m.Domains[0]; len(d.Addresses) != 1 || d.Entries[0].Address == "" || d.Entries[1].Address != "" {
		t.Errorf("有查看访问地址：范围内的入口给地址，范围外的仍然不给：%v", d)
	}
}

// 面板设置（B3、C3）和备用面板（B4）。
func TestCleanSettingsAndStandbys(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	full := func() *nagiv1.GetSettingsResponse {
		return &nagiv1.GetSettingsResponse{Settings: &nagiv1.PanelSettings{PublicAddress: "203.0.113.5", Port: 8443, Fingerprint: "fp",
			Version: "0.35", Domain: "nagi.example.com", MasterKeyPath: "/k", Name: "Nagi", DefaultName: "host"}}
	}
	m := full()
	a.filterReply(ctx, memberAccess(auth.PresetViewer), m)
	if s := m.Settings; s.PublicAddress != "" || s.Port != 0 || s.Fingerprint != "" || s.Domain != "" || s.MasterKeyPath != "" || s.DefaultName != "" || s.Version != "0.35" || s.Name != "Nagi" {
		t.Errorf("非管理员只给名称和版本：%v", s)
	}
	m = full()
	a.filterReply(ctx, memberAccess(auth.PermsOf(auth.PermSettings)), m)
	if m.Settings.Domain != "nagi.example.com" || m.Settings.PublicAddress != "" {
		t.Errorf("有面板设置时给域名：%v", m.Settings)
	}
	st := &nagiv1.GetStandbysResponse{Standbys: []*nagiv1.StandbyPanel{{Address: "203.0.113.6:8443"}, {Address: "backup.example.com:8443"}, {Address: "[2001:db8::1]:8443"}}}
	a.filterReply(ctx, memberAccess(auth.PresetViewer), st)
	if len(st.Standbys) != 1 || st.Standbys[0].Address != "backup.example.com:8443" {
		t.Errorf("只给绑了域名的备用面板：%v", st.Standbys)
	}
	st = &nagiv1.GetStandbysResponse{IsStandby: true, PrimaryAddress: "203.0.113.5:8443", LastError: "dial 203.0.113.5"}
	a.filterReply(ctx, memberAccess(auth.PresetViewer), st)
	if st.PrimaryAddress != "" || st.LastError != "" {
		t.Errorf("主面板只有 IP 时不给：%v", st)
	}
	app := &nagiv1.AppInfoResponse{PublicAddress: "203.0.113.5:8443", Domain: "nagi.example.com"}
	a.filterReply(ctx, memberAccess(auth.PresetViewer), app)
	if app.PublicAddress != "" {
		t.Error("绑了域名时 App 只拿到域名（B11）")
	}
}

// 部署输出、副本里看不到的节点名换成「其他节点」（A6、A7）。事件按引用换，见 TestActivityByRef。
func TestScrubNodeNames(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	x := memberAccess(auth.PresetOperator)
	ev := &nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: "迁移到 b 完成，a 上的容器已停止，b-2 不受影响"}}
	a.filterReply(ctx, x, ev)
	if got := ev.GetOutput(); got != "迁移到 其他节点 完成，a 上的容器已停止，b-2 不受影响" {
		t.Errorf("部署输出：%s", got)
	}
	rep := &nagiv1.ListNodeReplicasResponse{Replicas: []*nagiv1.NodeReplica{
		{Replica: &nagiv1.Replica{ProjectId: "p1", SourceNodeId: "n2", SourceName: "b", Dir: "/r/p1"}},
		{Replica: &nagiv1.Replica{ProjectId: "p2"}},
	}}
	a.filterReply(ctx, memberAccess(auth.PresetOperator|auth.PermsOf(auth.PermBackup)), rep)
	if len(rep.Replicas) != 1 || rep.Replicas[0].Replica.SourceNodeId != "" || rep.Replicas[0].Replica.SourceName != "" || rep.Replicas[0].Replica.Dir != "" {
		t.Errorf("副本：%v", rep.Replicas)
	}
	if got := scrubIPs("dial tcp 203.0.113.9:443 and [2001:db8::1]:80, v1.2.3 ok"); strings.Contains(got, "203.0.113.9") || strings.Contains(got, "2001:db8") || !strings.Contains(got, "v1.2.3") {
		t.Errorf("scrubIPs：%s", got)
	}
}

// 审计参数里地址类的键去掉（B9）。
func TestStripAddressParams(t *testing.T) {
	got := stripAddressParams(`{"issued_ip":"203.0.113.1","detail":"a","nested":{"address":"x","ok":1},"list":[{"endpoint":"e"}]}`)
	if strings.Contains(got, "203.0.113.1") || strings.Contains(got, `"address"`) || strings.Contains(got, "endpoint") || !strings.Contains(got, `"detail":"a"`) {
		t.Errorf("参数：%s", got)
	}
}

func protoregistryFind(name string) (protoreflect.MessageDescriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, protoregistry.NotFound
	}
	return md, nil
}

// 审计参数里看不到的节点 ID 去掉（A7）。
func TestScrubNodeIDs(t *testing.T) {
	a := cleanTest(t)
	x := memberAccess(auth.PresetOperator)
	got := a.scrubNodeIDs(context.Background(), x, `{"node_id":"n2","backup_node_ids":["n2","n1"]}`)
	if got != `{"node_id":"","backup_node_ids":["","n1"]}` {
		t.Errorf("参数：%s", got)
	}
}

// 回复里的 NodeInfo、NodeUsage 是节点上报的缓存（api.go 的 node 直接挂上 conn.LastReport() 里的指针），
// 清理回复不能改到它（2026-11-19-87c02b5#1）。
func TestCleanKeepsSharedReport(t *testing.T) {
	a := cleanTest(t)
	info := &nagiv1.NodeInfo{Hostname: "host-a", Os: "linux", Version: "0.34", ProjectsRoot: "/srv", ReplicaDir: "/r",
		DirectDownload: &nagiv1.DirectDownload{Address: "203.0.113.1"}}
	usage := &nagiv1.NodeUsage{Disks: []*nagiv1.DiskUsage{{Mount: "/data", TotalBytes: 10}}}
	wantInfo, wantUsage := proto.Clone(info), proto.Clone(usage)
	m := &nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{{Id: "n1", Name: "a", Online: true, Info: info, Usage: usage}}}
	a.filterReply(context.Background(), memberAccess(auth.PresetOperator), m)
	if len(m.Nodes) != 1 || m.Nodes[0].Info.Hostname != "" || m.Nodes[0].Usage.Disks[0].Mount != "" {
		t.Fatalf("回复应该清理过：%v", m)
	}
	if !proto.Equal(info, wantInfo) || !proto.Equal(usage, wantUsage) {
		t.Errorf("清理回复改到了共用的上报：%v %v", info, usage)
	}
}

// 文字清理的边角（2026-11-19-87c02b5#17）：末尾是 IPv4 的 IPv6、带 zone 的 IPv6。
func TestScrubEdges(t *testing.T) {
	for in, want := range map[string]string{
		"dial ::ffff:203.0.113.7: refused": "dial …: refused",
		"dial 203.0.113.7:443 failed":      "dial …:443 failed",
		"dial [2001:db8::1]:443 failed":    "dial …:443 failed",
		"at 12:30:45 done":                 "at 12:30:45 done",
	} {
		if got := scrubIPs(in); got != want {
			t.Errorf("scrubIPs(%q) = %q，想要 %q", in, got, want)
		}
	}
	for addr, want := range map[string]bool{"panel.example.com": true, "fe80::1%eth0": false, "[fe80::1%eth0]:9443": false, "203.0.113.7:9443": false} {
		if got := hostAddress(addr); got != want {
			t.Errorf("hostAddress(%q) = %v", addr, got)
		}
	}
}

// 报错里的节点只按引用换（i18n.Redact）：面板生成的「节点 xx 离线」这类报错，看不到的节点换成「其他节点」；
// 请求的人写进报错的文字不管怎么写都不换，所以不能借「换没换」探出有没有某个名称的节点
// （2026-11-19-7b50402#1、#2）。
func TestScrubErrorByRef(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	x := memberAccess(auth.PresetOperator)
	names := a.nodeNames(ctx, x)
	hiddenID, hiddenName := "", ""
	for id, n := range names {
		if !x.node(id) && n != "" {
			hiddenID, hiddenName = id, n
		}
	}
	if hiddenID == "" {
		t.Fatal("测试数据里应该有看不到的节点")
	}
	text := func(err error) string { return errorText(i18n.FromContext(ctx), err) }
	// 面板生成的报错：按引用换，错误码和响应头保留。
	gen := connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("节点 %s 版本过低", i18n.NodeRef(hiddenID, hiddenName)))
	gen.Meta().Set("X-Test", "kept")
	got := a.scrubError(ctx, x, gen)
	if text(got) != "节点 其他节点 版本过低" || connect.CodeOf(got) != connect.CodeFailedPrecondition {
		t.Errorf("面板生成的报错：%q %v", text(got), connect.CodeOf(got))
	}
	var ce *connect.Error
	if !errors.As(got, &ce) || ce.Meta().Get("X-Test") != "kept" {
		t.Error("响应头应该保留")
	}
	// 节点离线的错误，包在别的报错里也一样；errors.Is 照常能认。
	off := nodes.NewOfflineError(hiddenID, hiddenName)
	wrapped := connect.NewError(connect.CodeUnavailable, i18n.Errorf("同步失败：%w", off))
	got = a.scrubError(ctx, x, wrapped)
	if text(got) != "同步失败：节点 其他节点 离线" || !errors.Is(got, nodes.ErrOffline) {
		t.Errorf("节点离线：%q", text(got))
	}
	// 看得到的节点不换。
	visible := connect.NewError(connect.CodeUnavailable, nodes.NewOfflineError("n1", names["n1"]))
	if got := a.scrubError(ctx, x, visible); text(got) != "节点 "+names["n1"]+" 离线" {
		t.Errorf("看得到的节点不该换：%q", text(got))
	}
	// 请求的人写进报错的文字：整词、跟在换行后面、和别的词放在一起，都原样返回。
	for _, typed := range []string{hiddenName, "x" + string(rune(10)) + hiddenName, "a1 " + hiddenName + " c"} {
		echo := connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("权限 %s 不存在", typed))
		if got := a.scrubError(ctx, x, echo); got != error(echo) {
			t.Errorf("请求里写的 %q 不该让报错被改：%q", typed, text(got))
		}
	}
	// 管理员不换。
	if got := a.scrubError(ctx, nil, gen); got != error(gen) {
		t.Error("管理员的报错不该改")
	}
}

// MCP 调用的记录：有看不到的节点时，非管理员不看失败详情，参数里和节点有关的键去掉（2026-11-19-834f027#2）。
func TestStripParamsNodeKeys(t *testing.T) {
	in := `{"arguments":{"project":"web","node":"prod-9","backup_node":"prod-8","items":[{"target_node":"x","keep":1}]},"result_bytes":3}`
	got := stripParams(in, func(k string) bool { return strings.Contains(strings.ToLower(k), "node") })
	if strings.Contains(got, "prod-9") || strings.Contains(got, "prod-8") || strings.Contains(got, "target_node") {
		t.Errorf("和节点有关的键应该去掉：%s", got)
	}
	if !strings.Contains(got, `"project":"web"`) || !strings.Contains(got, `"keep":1`) || !strings.Contains(got, "result_bytes") {
		t.Errorf("别的键应该保留：%s", got)
	}
}
