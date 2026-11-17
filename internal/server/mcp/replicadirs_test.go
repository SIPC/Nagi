package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 副本存放位置的工具等于节点的 root：只给管理员、令牌勾了「可修改」和「节点终端」，备用面板上没有。
func TestReplicaDirToolsAllowed(t *testing.T) {
	full := auth.User{TokenID: 1, Role: auth.RoleAdmin, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	noNode := full
	noNode.AllowNodeExec = false
	noWrite := full
	noWrite.AllowWrite = false
	operator := full
	operator.Role = auth.RoleOperator
	for _, tl := range replicaDirTools {
		if !tl.write || !tl.nodeExec || !tl.adminOnly || tl.scope() != "nagi:node" {
			t.Errorf("%s 的权限声明不对", tl.name)
		}
		if !slicesContains(allTools, tl.name) {
			t.Errorf("%s 没有登记到工具列表", tl.name)
		}
		for i, c := range []struct {
			u       auth.User
			standby bool
			want    bool
		}{{full, false, true}, {full, true, false}, {noNode, false, false}, {noWrite, false, false}, {operator, false, false}} {
			if got := tl.allowedFor(c.u, c.standby); got != c.want {
				t.Errorf("%s case %d：allowedFor = %v，想要 %v", tl.name, i, got, c.want)
			}
		}
	}
}

func slicesContains(list []tool, name string) bool {
	for _, t := range list {
		if t.name == name {
			return true
		}
	}
	return false
}

// 确认码绑定工具、节点、容器和目录：预览之后改了哪一项都不能用原来的确认码执行。
func TestReplicaDirCode(t *testing.T) {
	c, _ := replicaTestCall(t)
	code := func(parts ...string) string {
		return c.confirmCode(replicaDirParts(parts[0], parts[1], parts[2], parts[3], parts[4])...)
	}
	base := code("set_replica_location", "n1", "p1", "web", "/mnt/big")
	for i, other := range []string{
		code("set_node_replica_dir", "n1", "p1", "web", "/mnt/big"),
		code("set_replica_location", "n2", "p1", "web", "/mnt/big"),
		code("set_replica_location", "n1", "p2", "web", "/mnt/big"),
		code("set_replica_location", "n1", "p1", "db", "/mnt/big"),
		code("set_replica_location", "n1", "p1", "web", "/mnt/other"),
		code("set_replica_location", "n1", "p1", "web", ""),
	} {
		if other == base {
			t.Errorf("case %d：参数变了，确认码却一样", i)
		}
	}
}

// fakeReplicaAPI 模拟面板接口：两个节点（n2 支持选择存放位置，n3 是旧版本），一个容器 web，备份节点是 n2、n3。
type fakeReplicaAPI struct {
	nagiv1connect.UnimplementedNodeServiceHandler
	nagiv1connect.UnimplementedProjectServiceHandler
	setNode []*nagiv1.SetNodeReplicaDirRequest
	setDir  []*nagiv1.SetBackupReplicaDirRequest
}

func (f *fakeReplicaAPI) ListNodes(context.Context, *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{
		{Id: "n2", Name: "big", Info: &nagiv1.NodeInfo{ReplicaDirs: true}},
		{Id: "n3", Name: "old", Info: &nagiv1.NodeInfo{}},
	}}), nil
}

func (f *fakeReplicaAPI) ListNodeReplicas(context.Context, *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.ListNodeReplicasResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodeReplicasResponse{Replicas: []*nagiv1.NodeReplica{
		{Replica: &nagiv1.Replica{ProjectName: "shop", Container: "web"}},
		{Replica: &nagiv1.Replica{ProjectName: "shop", Container: "db", CustomDir: true}},
	}}), nil
}

func (f *fakeReplicaAPI) SetNodeReplicaDir(_ context.Context, req *connect.Request[nagiv1.SetNodeReplicaDirRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	f.setNode = append(f.setNode, req.Msg)
	return out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: &nagiv1.DeployResult{Success: true}}})
}

var replicaProject = &nagiv1.Project{Id: "p1", Name: "shop", NetworkName: "shop", Containers: []*nagiv1.Container{
	{Name: "web", NodeId: "n1", BackupNodeIds: []string{"n2", "n3"}, BackupReplicaDirs: map[string]string{"n2": "/mnt/old"}},
}}

func (f *fakeReplicaAPI) ListProjects(context.Context, *connect.Request[nagiv1.ListProjectsRequest]) (*connect.Response[nagiv1.ListProjectsResponse], error) {
	return connect.NewResponse(&nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{{Project: replicaProject}}}), nil
}

func (f *fakeReplicaAPI) GetProject(context.Context, *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.ProjectSummary], error) {
	return connect.NewResponse(&nagiv1.ProjectSummary{Project: replicaProject}), nil
}

func (f *fakeReplicaAPI) SetBackupReplicaDir(_ context.Context, req *connect.Request[nagiv1.SetBackupReplicaDirRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	f.setDir = append(f.setDir, req.Msg)
	return out.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: &nagiv1.DeployResult{Success: true}}})
}

func replicaTestCall(t *testing.T) (*call, *fakeReplicaAPI) {
	t.Helper()
	f := &fakeReplicaAPI{}
	mux := http.NewServeMux()
	mux.Handle(nagiv1connect.NewNodeServiceHandler(f))
	mux.Handle(nagiv1connect.NewProjectServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &handler{clients: clients{
		nodes:    nagiv1connect.NewNodeServiceClient(srv.Client(), srv.URL),
		projects: nagiv1connect.NewProjectServiceClient(srv.Client(), srv.URL),
	}}
	u := auth.User{TokenID: 1, Role: auth.RoleAdmin, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	return &call{ctx: context.Background(), u: u, h: h, req: &mcp.CallToolRequest{}}, f
}

// 不带确认码、确认码对不上、参数改了，都只返回预览，不调用接口；带上预览里的确认码才执行。
func TestSetNodeReplicaDirConfirm(t *testing.T) {
	c, f := replicaTestCall(t)
	out, err := toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "big", Dir: " /mnt/data/ "})
	if err != nil {
		t.Fatal(err)
	}
	p := out.(map[string]any)
	if p["preview"] == nil || len(f.setNode) != 0 {
		t.Fatalf("第一次调用只应该返回预览：%v", p)
	}
	if moving := p["replicas_to_move"].([]string); len(moving) != 1 || moving[0] != "shop / web" {
		t.Fatalf("预览里要列出会移动的副本（单独指定了位置的不动）：%v", moving)
	}
	code := c.confirmCode(replicaDirParts("set_node_replica_dir", "n2", "", "", "/mnt/data")...)
	if out, _ := toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "big", Dir: "/mnt/data", Confirm: "000000000000"}); out.(map[string]any)["note"] == nil || len(f.setNode) != 0 {
		t.Fatal("确认码不对时不能执行")
	}
	if _, _ = toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "big", Dir: "/mnt/other", Confirm: code}); len(f.setNode) != 0 {
		t.Fatal("预览之后改了目录，原来的确认码不能用")
	}
	res, err := toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "big", Dir: "/mnt/data/", Confirm: code})
	if err != nil || !res.(opResult).Success {
		t.Fatalf("带上确认码应该执行：%v %v", res, err)
	}
	if len(f.setNode) != 1 || f.setNode[0].GetNodeId() != "n2" || f.setNode[0].GetDir() != "/mnt/data" {
		t.Fatalf("调用的参数不对：%v", f.setNode)
	}
	// 旧版本的节点直接拒绝，不给预览；相对路径拒绝。
	if _, err := toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "old", Dir: "/mnt/data"}); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("旧版本的节点应该拒绝：%v", err)
	}
	if _, err := toolSetNodeReplicaDir(c, SetNodeReplicaDirArgs{Node: "big", Dir: "mnt/../etc"}); err == nil {
		t.Fatal("相对路径应该拒绝")
	}
}

func TestSetReplicaLocationConfirm(t *testing.T) {
	c, f := replicaTestCall(t)
	arg := SetReplicaLocationArgs{ContainerArg: ContainerArg{Project: "shop", Container: "web"}, Node: "big", Dir: "/mnt/big"}
	out, err := toolSetReplicaLocation(c, arg)
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(map[string]any); p["preview"] == nil || p["current"] != "/mnt/old" || len(f.setDir) != 0 {
		t.Fatalf("第一次调用只应该返回预览：%v", p)
	}
	arg.Confirm = c.confirmCode(replicaDirParts("set_replica_location", "n2", "p1", "web", "/mnt/big")...)
	res, err := toolSetReplicaLocation(c, arg)
	if err != nil || !res.(opResult).Success || len(f.setDir) != 1 || f.setDir[0].GetDir() != "/mnt/big" || f.setDir[0].GetNodeId() != "n2" {
		t.Fatalf("带上确认码应该执行：%v %v %v", res, err, f.setDir)
	}
	// 清空（移回节点的副本目录）要单独确认：原来的确认码不能用。
	arg.Dir = ""
	if out, _ := toolSetReplicaLocation(c, arg); out.(map[string]any)["preview"] == nil || len(f.setDir) != 1 {
		t.Fatal("参数变了要重新预览")
	}
	// 不是备份节点、旧版本的节点直接拒绝。
	arg.Node, arg.Dir, arg.Confirm = "n1", "/mnt/big", ""
	if _, err := toolSetReplicaLocation(c, arg); err == nil {
		t.Fatal("不存在或不是备份节点的应该拒绝")
	}
	arg.Node = "old"
	if _, err := toolSetReplicaLocation(c, arg); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("旧版本的节点应该拒绝：%v", err)
	}
}
