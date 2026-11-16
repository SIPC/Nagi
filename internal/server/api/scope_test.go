package api

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 操作员、只读成员能调用的项目和节点接口，都要能按访问范围核对：请求里有项目或节点，或者登记在 scopeFiltered。
// 新增接口忘了处理时这里会失败。
func TestScopeCoversMethods(t *testing.T) {
	for _, name := range []string{"ProjectService", "NodeService"} {
		svc := nagiv1.File_nagi_v1_api_proto.Services().ByName(protoreflect.Name(name))
		for i := range svc.Methods().Len() {
			md := svc.Methods().Get(i)
			procedure := "/nagi.v1." + name + "/" + string(md.Name())
			if !auth.Allowed(auth.RoleOperator, procedure) && !auth.Allowed(auth.RoleViewer, procedure) {
				continue
			}
			if scopeFiltered[procedure] {
				continue
			}
			fields := md.Input().Fields()
			ok := false
			for j := range fields.Len() {
				if n := fields.Get(j).Name(); scopeProjectFields[n] || scopeNodeFields[n] {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s 没有按访问范围核对：请求里没有项目或节点，也没有登记在 scopeFiltered", procedure)
			}
		}
	}
}

// extra 是额外的建表数据。
func scopeTestAPI(t *testing.T, extra ...string) (*API, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range append([]string{
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (2, 'op', 'x', 0, 'operator', 0, 0)`,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'a', 0, x'01'), ('n2', 'b', 0, x'02')`,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x''), ('p2', 'two', 'two', 1, 0, x'')`,
		`INSERT INTO user_projects (user_id, project_id) VALUES (2, 'p1')`,
		`INSERT INTO user_nodes (user_id, node_id) VALUES (2, 'n1')`,
	}, extra...) {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	q := store.New(d)
	return &API{Q: q}, q
}

func TestScopeCheckRequest(t *testing.T) {
	a, _ := scopeTestAPI(t)
	ctx := context.Background()
	x, err := a.loadAccess(ctx, auth.User{ID: 2, Role: auth.RoleOperator, LimitProjects: true, LimitNodes: true})
	if err != nil || x == nil {
		t.Fatal(x, err)
	}
	if y, _ := a.loadAccess(ctx, auth.User{ID: 2, Role: auth.RoleAdmin, LimitProjects: true}); y != nil {
		t.Error("管理员不受访问范围限制")
	}
	cases := []struct {
		procedure string
		msg       proto.Message
		want      connect.Code
	}{
		{"/nagi.v1.ProjectService/GetProject", &nagiv1.ProjectRef{ProjectId: "p1"}, 0},
		{"/nagi.v1.ProjectService/GetProject", &nagiv1.ProjectRef{ProjectId: "p2"}, connect.CodeNotFound},
		{"/nagi.v1.ProjectService/AddContainer", &nagiv1.AddContainerRequest{ProjectId: "p1", NodeId: "n1"}, 0},
		{"/nagi.v1.ProjectService/AddContainer", &nagiv1.AddContainerRequest{ProjectId: "p1", NodeId: "n2"}, connect.CodeNotFound},
		{"/nagi.v1.ProjectService/ConnectProject", &nagiv1.ConnectProjectRequest{ProjectId: "p1", OtherProjectId: "p2"}, connect.CodeNotFound},
		// 见证人为空表示面板。
		{"/nagi.v1.ProjectService/SetFailover", &nagiv1.SetFailoverApiRequest{ProjectId: "p1", StandbyNodeId: "n1"}, 0},
		{"/nagi.v1.ProjectService/SetFailover", &nagiv1.SetFailoverApiRequest{ProjectId: "p1", StandbyNodeId: "n1", WitnessNodeId: "n2"}, connect.CodeNotFound},
		{"/nagi.v1.NodeService/QueryNodeMetrics", &nagiv1.QueryNodeMetricsRequest{NodeId: "n2"}, connect.CodeNotFound},
		// 范围外节点上本项目容器的占用可以查，别的不行。
		{"/nagi.v1.NodeService/QueryNodeMetrics", &nagiv1.QueryNodeMetricsRequest{NodeId: "n2", Prefix: "c:p1/"}, 0},
		{"/nagi.v1.NodeService/QueryNodeMetrics", &nagiv1.QueryNodeMetricsRequest{NodeId: "n2", Prefix: "c:p2/"}, connect.CodeNotFound},
		{"/nagi.v1.NodeService/QueryNodeMetrics", &nagiv1.QueryNodeMetricsRequest{NodeId: "n2", Prefix: "c:"}, connect.CodeNotFound},
		{"/nagi.v1.NodeService/PingNodeMesh", &nagiv1.PingNodeMeshRequest{NodeId: "n1", PublicKey: []byte{2}}, connect.CodeNotFound},
		{"/nagi.v1.NodeService/PingNodeMesh", &nagiv1.PingNodeMeshRequest{NodeId: "n1", PublicKey: []byte{9}}, 0},
	}
	for _, c := range cases {
		err := a.checkRequest(ctx, x, c.procedure, c.msg)
		if got := connect.CodeOf(err); err == nil && c.want != 0 || err != nil && got != c.want {
			t.Errorf("%s %v：得到 %v，想要 %v", c.procedure, c.msg, err, c.want)
		}
	}
}

// 映射的入口节点藏在 route 里面，也要按范围核对；映射原来就有的入口节点（管理员设的）可以留着。
func TestScopeEntryNodes(t *testing.T) {
	a, _ := scopeTestAPI(t,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub)
		 VALUES ('p1', 'web', 1, 'n1', '/srv/web', 1, 0, 'h', x'', '')`,
		`INSERT INTO routes (project_id, name, container, service, target, protocol, entry_nodes) VALUES ('p1', 'web-80', 'web', '', 80, 'tcp', '["n2"]')`,
	)
	ctx := context.Background()
	x, err := a.loadAccess(ctx, auth.User{ID: 2, Role: auth.RoleOperator, LimitProjects: true, LimitNodes: true})
	if err != nil || x == nil {
		t.Fatal(x, err)
	}
	route := func(name string, nodes ...string) *nagiv1.Exposure {
		return &nagiv1.Exposure{Name: name, Container: "web", Target: 80, Protocol: "tcp", EntryNodes: nodes}
	}
	cases := []struct {
		name string
		msg  proto.Message
		want connect.Code
	}{
		{"范围内", &nagiv1.SaveRouteRequest{ProjectId: "p1", Route: route("", "n1")}, 0},
		{"新映射用范围外的节点", &nagiv1.SaveRouteRequest{ProjectId: "p1", Route: route("", "n2")}, connect.CodeNotFound},
		{"别的映射用范围外的节点", &nagiv1.SaveRouteRequest{ProjectId: "p1", Route: route("other", "n2")}, connect.CodeNotFound},
		{"原来就有的入口节点", &nagiv1.SaveRouteRequest{ProjectId: "p1", Route: route("web-80", "n2", "n1")}, 0},
		{"检查域名", &nagiv1.CheckDomainsRequest{ProjectId: "p1", Routes: []*nagiv1.Exposure{route("", "n2")}}, connect.CodeNotFound},
	}
	for _, c := range cases {
		procedure := "/nagi.v1.ProjectService/SaveRoute"
		if _, ok := c.msg.(*nagiv1.CheckDomainsRequest); ok {
			procedure = "/nagi.v1.ProjectService/CheckDomains"
		}
		err := a.checkRequest(ctx, x, procedure, c.msg)
		if got := connect.CodeOf(err); err == nil && c.want != 0 || err != nil && got != c.want {
			t.Errorf("%s：得到 %v，想要 %v", c.name, err, c.want)
		}
	}
}

func TestScopeFilterReply(t *testing.T) {
	a, _ := scopeTestAPI(t)
	ctx := context.Background()
	x := &access{projects: map[string]bool{"p1": true}, nodes: map[string]bool{"n1": true}}

	projects := &nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{
		{Project: &nagiv1.Project{Id: "p1", Links: []*nagiv1.LinkedProject{{ProjectId: "p2", Name: "two", NetworkName: "two"}}}},
		{Project: &nagiv1.Project{Id: "p2"}},
	}}
	a.filterReply(ctx, x, projects)
	if len(projects.Projects) != 1 || projects.Projects[0].Project.Id != "p1" {
		t.Fatalf("范围外的项目没有去掉：%v", projects)
	}
	if l := projects.Projects[0].Project.Links[0]; l.Name != "" || l.NetworkName != "two" {
		t.Errorf("范围外的已连接项目只去掉名称：%v", l)
	}

	mesh := &nagiv1.MeshStatus{Peers: []*nagiv1.MeshPeerStatus{{PublicKey: []byte{1}}, {PublicKey: []byte{2}}, {PublicKey: []byte{9}, Name: "panel"}}}
	a.filterReply(ctx, x, mesh)
	if len(mesh.Peers) != 2 || mesh.Peers[0].PublicKey[0] != 1 || mesh.Peers[1].Name != "panel" {
		t.Errorf("mesh 成员只去掉范围外的节点：%v", mesh.Peers)
	}

	metrics := &nagiv1.QueryMetricsResponse{Series: []*nagiv1.MetricSeries{
		{Name: "node:net_rx"}, {Name: "c:p1/web/web:cpu"}, {Name: "c:p2/web/web:cpu"},
		{Name: "r:p1/web:req"}, {Name: "r:p2/web:req"},
		{Name: "l:p1/web:cpu"}, {Name: "l:p2/web:cpu"}, {Name: "snapshot:p2/web"}, {Name: "new:x"},
		{Name: "m:01:rx"}, {Name: "m:02:rx"}, {Name: "m:02:tx"},
	}}
	a.filterReply(ctx, x, metrics)
	var names []string
	for _, s := range metrics.Series {
		names = append(names, s.Name)
	}
	if got, want := strings.Join(names, " "), "node:net_rx c:p1/web/web:cpu r:p1/web:req l:p1/web:cpu m:01:rx m:0:rx m:0:tx"; got != want {
		t.Errorf("监控序列：%s，想要 %s", got, want)
	}
	if strings.Contains(strings.Join(names, " "), hex.EncodeToString([]byte{2})+":") {
		t.Error("范围外节点的公钥不该出现")
	}
}

// 删除项目、移除节点时，从成员的访问范围里去掉。
func TestScopeCleanup(t *testing.T) {
	_, q := scopeTestAPI(t)
	ctx := context.Background()
	if err := q.DeleteProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteNodeFromScopes(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := q.ListUserProjects(ctx, 2); len(ids) != 0 {
		t.Errorf("项目删除后还在范围里：%v", ids)
	}
	if ids, _ := q.ListUserNodes(ctx, 2); len(ids) != 0 {
		t.Errorf("节点移除后还在范围里：%v", ids)
	}
}

// 访问范围里的分组和标签：分组里的项目、带标签的节点都在范围内；重命名跟着改，删除后不再算。
func TestScopeGroupsAndTags(t *testing.T) {
	a, q := scopeTestAPI(t,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p3', 'three', 'three', 1, 0, x'')`,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n3', 'c', 0, x'03')`,
	)
	ctx := context.Background()
	u := auth.User{ID: 2, Role: auth.RoleOperator, LimitProjects: true, LimitNodes: true}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.SetProjectGroup(ctx, "p2", "web"))
	must(q.SetProjectGroup(ctx, "p3", "db"))
	must(q.SetNodeTags(ctx, "n2", []string{"hk", "edge"}))
	must(q.SetNodeTags(ctx, "n3", []string{"us"}))
	must(q.SetUserProjectGroups(ctx, 2, []string{"web"}))
	must(q.SetUserNodeTags(ctx, 2, []string{"edge"}))

	check := func(projects, nodes map[string]bool) {
		t.Helper()
		x, err := a.loadAccess(ctx, u)
		must(err)
		for id, want := range projects {
			if x.project(id) != want {
				t.Errorf("项目 %s 在范围内：%v，想要 %v", id, x.project(id), want)
			}
		}
		for id, want := range nodes {
			if x.node(id) != want {
				t.Errorf("节点 %s 在范围内：%v，想要 %v", id, x.node(id), want)
			}
		}
	}
	check(map[string]bool{"p1": true, "p2": true, "p3": false}, map[string]bool{"n1": true, "n2": true, "n3": false})

	// 以后放进分组、打上标签的也算。
	must(q.SetProjectGroup(ctx, "p3", "web"))
	must(q.SetNodeTags(ctx, "n3", []string{"edge"}))
	check(map[string]bool{"p3": true}, map[string]bool{"n3": true})

	// 重命名：范围跟着改，合并到已有的名称也一样。
	must(q.RenameProjectGroup(ctx, "web", "apps"))
	must(q.RenameNodeTag(ctx, "edge", "hk"))
	check(map[string]bool{"p2": true, "p3": true}, map[string]bool{"n2": true, "n3": true})
	if g, _ := q.ListUserProjectGroups(ctx, 2); len(g) != 1 || g[0] != "apps" {
		t.Errorf("重命名后范围里的分组：%v", g)
	}
	if tags, _ := q.NodeTags(ctx, "n2"); len(tags) != 1 || tags[0] != "hk" {
		t.Errorf("合并后节点的标签：%v", tags)
	}
	// 同名重命名不改变任何东西。
	must(q.RenameNodeTag(ctx, "hk", "hk"))
	check(nil, map[string]bool{"n2": true})

	// 删除：项目变成未分组，节点去掉标签，都不在范围内了。
	must(q.RenameProjectGroup(ctx, "apps", ""))
	must(q.RenameNodeTag(ctx, "hk", ""))
	check(map[string]bool{"p1": true, "p2": false, "p3": false}, map[string]bool{"n1": true, "n2": false, "n3": false})
	if g, _ := q.ProjectGroup(ctx, "p2"); g != "" {
		t.Errorf("删除分组后项目还在分组 %q 里", g)
	}
	if g, _ := q.ListUserProjectGroups(ctx, 2); len(g) != 0 {
		t.Errorf("删除分组后还在范围里：%v", g)
	}
}

// 分组只有管理员能改：它决定成员的访问范围。
func TestGroupAdminOnly(t *testing.T) {
	for _, p := range []string{"/nagi.v1.ProjectService/SetProjectGroup", "/nagi.v1.ProjectService/RenameProjectGroup",
		"/nagi.v1.NodeService/SetNodeTags", "/nagi.v1.NodeService/RenameNodeTag"} {
		if auth.Allowed(auth.RoleOperator, p) || auth.Allowed(auth.RoleViewer, p) {
			t.Errorf("%s 只有管理员能用", p)
		}
		if !auth.Allowed(auth.RoleAdmin, p) {
			t.Errorf("%s 管理员要能用", p)
		}
	}
}

// 备份节点：新加的要在范围内，原来就有的（管理员设的）可以留着；从备份恢复时恢复到的节点要在范围内。
func TestScopeBackupNodes(t *testing.T) {
	a, _ := scopeTestAPI(t,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n3', 'c', 0, x'03')`,
		`INSERT INTO user_nodes (user_id, node_id) VALUES (2, 'n3')`,
		`INSERT INTO containers (project_id, name, position, node_id, dir, revision, created_at, webhook_hash, secrets, deploy_key_pub, backup_node_id)
		 VALUES ('p1', 'web', 1, 'n1', '/srv/web', 1, 0, 'h', x'', '', 'n2')`,
	)
	ctx := context.Background()
	x, err := a.loadAccess(ctx, auth.User{ID: 2, Role: auth.RoleOperator, LimitProjects: true, LimitNodes: true})
	if err != nil || x == nil {
		t.Fatal(x, err)
	}
	set := func(ids ...string) proto.Message {
		return &nagiv1.SetBackupNodesRequest{ProjectId: "p1", Container: "web", BackupNodeIds: ids}
	}
	cases := []struct {
		name      string
		procedure string
		msg       proto.Message
		want      connect.Code
	}{
		{"加范围内的节点，原来的留着", "SetBackupNodes", set("n2", "n3"), 0},
		{"去掉原来的", "SetBackupNodes", set("n3"), 0},
		{"加范围外的节点", "SetBackupNodes", set("n2", "nx"), connect.CodeNotFound},
		{"在范围外的备份节点上恢复", "RestoreFromBackup", &nagiv1.RestoreFromBackupRequest{ProjectId: "p1", Container: "web", NodeId: "n2"}, connect.CodeNotFound},
		{"不指定节点：有备份节点在范围外", "RestoreFromBackup", &nagiv1.RestoreFromBackupRequest{ProjectId: "p1", Container: "web"}, connect.CodeNotFound},
		{"同步到范围外的备份节点", "SyncBackup", &nagiv1.BackupNodeRef{ProjectId: "p1", Container: "web", NodeId: "n2"}, connect.CodeNotFound},
		{"同步全部", "SyncBackup", &nagiv1.BackupNodeRef{ProjectId: "p1", Container: "web"}, 0},
	}
	for _, c := range cases {
		err := a.checkRequest(ctx, x, "/nagi.v1.ProjectService/"+c.procedure, c.msg)
		if got := connect.CodeOf(err); err == nil && c.want != 0 || err != nil && got != c.want {
			t.Errorf("%s：得到 %v，想要 %v", c.name, err, c.want)
		}
	}
}

// L16：靠分组授权的成员，之前在审计页看不到分组里的项目的记录（审计页只读直接给的项目）。
// 现在审计页和其他页面用同一个访问范围。
func TestAuditViewerIncludesGroups(t *testing.T) {
	a, q := scopeTestAPI(t,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p3', 'three', 'three', 1, 0, x'')`,
	)
	ctx := context.Background()
	if err := q.SetProjectGroup(ctx, "p3", "web"); err != nil {
		t.Fatal(err)
	}
	if err := q.SetUserProjectGroups(ctx, 2, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	u := auth.User{ID: 2, Role: auth.RoleOperator, LimitProjects: true}
	v, err := a.auditViewer(auth.WithUser(ctx, u))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(v.projects, "p3") || !slices.Contains(v.projects, "p1") {
		t.Fatalf("审计页的访问范围应包括分组里的项目：%v", v.projects)
	}
	if slices.Contains(v.projects, "p2") {
		t.Fatalf("范围外的项目不应出现：%v", v.projects)
	}
}
