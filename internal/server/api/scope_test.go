package api

import (
	"context"
	"encoding/hex"
	"path/filepath"
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

func scopeTestAPI(t *testing.T) (*API, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (2, 'op', 'x', 0, 'operator', 0, 0)`,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'a', 0, x'01'), ('n2', 'b', 0, x'02')`,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x''), ('p2', 'two', 'two', 1, 0, x'')`,
		`INSERT INTO user_projects (user_id, project_id) VALUES (2, 'p1')`,
		`INSERT INTO user_nodes (user_id, node_id) VALUES (2, 'n1')`,
	} {
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
