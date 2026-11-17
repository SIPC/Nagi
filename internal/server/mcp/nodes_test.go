package mcp

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 假的节点和项目接口：只记下调用了哪些修改接口和参数，用来检查工具先预览、确认码对上才执行。
type fakeNodeAdmin struct {
	nagiv1connect.UnimplementedNodeServiceHandler
	mu    sync.Mutex
	calls []string
	write *nagiv1.WriteNodeFileRequest
	del   *nagiv1.DeleteNodeFileRequest
	move  *nagiv1.NodeTransferRequest
}

func (f *fakeNodeAdmin) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeNodeAdmin) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNodeAdmin) ListNodes(context.Context, *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{
		{Id: "n1", Name: "alpha", Online: true, Containers: 0, Info: &nagiv1.NodeInfo{Version: "v1"}},
		{Id: "n2", Name: "beta", Online: true, Pending: true, Info: &nagiv1.NodeInfo{Version: "v1", Hostname: "beta-host"}},
	}}), nil
}

func (f *fakeNodeAdmin) ListNodeDir(_ context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.ListNodeDirResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodeDirResponse{Path: req.Msg.GetPath(), Access: "normal", Entries: []*nagiv1.NodeFileEntry{
		{Name: "app.conf", Size: 3, Access: "normal"},
		{Name: "shadow", Size: 3, Access: "confirm"},
		{Name: "data", IsDir: true, Access: "normal"},
	}}), nil
}

func (f *fakeNodeAdmin) WriteNodeFile(_ context.Context, req *connect.Request[nagiv1.WriteNodeFileRequest]) (*connect.Response[nagiv1.WriteNodeFileResponse], error) {
	f.record("WriteNodeFile")
	f.mu.Lock()
	f.write = req.Msg
	f.mu.Unlock()
	return connect.NewResponse(&nagiv1.WriteNodeFileResponse{Sha256: digest(req.Msg.GetContent())}), nil
}

func (f *fakeNodeAdmin) DeleteNodeFile(_ context.Context, req *connect.Request[nagiv1.DeleteNodeFileRequest]) (*connect.Response[nagiv1.NodeFileOpResponse], error) {
	f.record("DeleteNodeFile")
	f.mu.Lock()
	f.del = req.Msg
	f.mu.Unlock()
	return connect.NewResponse(&nagiv1.NodeFileOpResponse{}), nil
}

func (f *fakeNodeAdmin) MoveNodeFiles(_ context.Context, req *connect.Request[nagiv1.NodeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	f.record("MoveNodeFiles")
	f.mu.Lock()
	f.move = req.Msg
	f.mu.Unlock()
	if err := out.Send(&nagiv1.FileOpEvent{Files: 1, Bytes: 3}); err != nil {
		return err
	}
	return out.Send(&nagiv1.FileOpEvent{Result: &nagiv1.FileOpResult{Paths: []string{"/srv/x"}}})
}

func (f *fakeNodeAdmin) RemoveNode(context.Context, *connect.Request[nagiv1.RemoveNodeRequest]) (*connect.Response[nagiv1.RemoveNodeResponse], error) {
	f.record("RemoveNode")
	return connect.NewResponse(&nagiv1.RemoveNodeResponse{}), nil
}

func (f *fakeNodeAdmin) ApproveNode(context.Context, *connect.Request[nagiv1.ApproveNodeRequest]) (*connect.Response[nagiv1.ApproveNodeResponse], error) {
	f.record("ApproveNode")
	return connect.NewResponse(&nagiv1.ApproveNodeResponse{}), nil
}

func (f *fakeNodeAdmin) IsolateNode(_ context.Context, req *connect.Request[nagiv1.IsolateNodeRequest]) (*connect.Response[nagiv1.IsolateNodeResponse], error) {
	f.record("IsolateNode:" + req.Msg.GetConfirmName())
	return connect.NewResponse(&nagiv1.IsolateNodeResponse{}), nil
}

type fakeProjectAdmin struct {
	nagiv1connect.UnimplementedProjectServiceHandler
	mu    sync.Mutex
	calls []string
}

func (f *fakeProjectAdmin) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func testProject() *nagiv1.ProjectSummary {
	return &nagiv1.ProjectSummary{Project: &nagiv1.Project{Id: "p1", Name: "shop", NetworkName: "shop", Revision: 3,
		Containers: []*nagiv1.Container{{Name: "web", NodeId: "n1"}}}}
}

func (f *fakeProjectAdmin) ListProjects(context.Context, *connect.Request[nagiv1.ListProjectsRequest]) (*connect.Response[nagiv1.ListProjectsResponse], error) {
	return connect.NewResponse(&nagiv1.ListProjectsResponse{Projects: []*nagiv1.ProjectSummary{testProject()}}), nil
}

func (f *fakeProjectAdmin) GetProject(context.Context, *connect.Request[nagiv1.ProjectRef]) (*connect.Response[nagiv1.ProjectSummary], error) {
	return connect.NewResponse(testProject()), nil
}

func (f *fakeProjectAdmin) MoveVolumeFiles(_ context.Context, req *connect.Request[nagiv1.VolumeTransferRequest], out *connect.ServerStream[nagiv1.FileOpEvent]) error {
	f.record("MoveVolumeFiles:" + req.Msg.GetConflict())
	if req.Msg.GetConflict() == "" {
		return out.Send(&nagiv1.FileOpEvent{Result: &nagiv1.FileOpResult{Conflicts: []string{"a.txt"}}})
	}
	return out.Send(&nagiv1.FileOpEvent{Result: &nagiv1.FileOpResult{Paths: []string{"dst/a.txt"}}})
}

func nodeAdminCall(t *testing.T) (*call, *fakeNodeAdmin, *fakeProjectAdmin) {
	t.Helper()
	fn, fp := &fakeNodeAdmin{}, &fakeProjectAdmin{}
	mux := http.NewServeMux()
	mux.Handle(nagiv1connect.NewNodeServiceHandler(fn))
	mux.Handle(nagiv1connect.NewProjectServiceHandler(fp))
	hc := &http.Client{Transport: inProcess{mux}}
	const base = "http://nagi.internal"
	h := &handler{o: Options{Version: "v2"}, clients: clients{
		projects: nagiv1connect.NewProjectServiceClient(hc, base),
		nodes:    nagiv1connect.NewNodeServiceClient(hc, base),
	}}
	u := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	return &call{ctx: context.Background(), u: u, h: h, req: &mcp.CallToolRequest{}}, fn, fp
}

func previewCode(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok || m["preview"] == nil {
		t.Fatalf("应该返回预览：%#v", v)
	}
	next, _ := m["next"].(string)
	i := strings.LastIndex(next, "confirm set to \"")
	if i < 0 {
		t.Fatalf("预览里没有确认码：%q", next)
	}
	return strings.TrimSuffix(next[i+len("confirm set to \""):], "\"")
}

// 改节点文件每次都要先预览：不带确认码、确认码对不上（参数变了）都只返回预览，对上了才写，并且带上 confirmed。
func TestWriteNodeFileConfirm(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	in := WriteNodeFileArgs{Node: "alpha", Path: "/etc/app.conf", Content: "new", ExpectedSHA256: digest([]byte("old"))}
	v, err := toolWriteNodeFile(c, in)
	if err != nil {
		t.Fatal(err)
	}
	code := previewCode(t, v)
	changed := in
	changed.Content, changed.Confirm = "other", code
	if v, err := toolWriteNodeFile(c, changed); err != nil || v.(map[string]any)["preview"] == nil {
		t.Fatalf("内容变了，原来的确认码不能用：%v %v", v, err)
	}
	if len(fn.called()) != 0 {
		t.Fatalf("预览阶段不该写：%v", fn.called())
	}
	in.Confirm = code
	if _, err := toolWriteNodeFile(c, in); err != nil {
		t.Fatal(err)
	}
	if fn.write == nil || !fn.write.GetConfirmed() || fn.write.GetCreate() || fn.write.GetExpectedSha256() != in.ExpectedSHA256 {
		t.Fatalf("写入的参数不对：%v", fn.write)
	}
	// 已有的文件不带 sha256 拒绝；相对路径、超过 1 MB、二进制内容拒绝。
	for _, bad := range []WriteNodeFileArgs{
		{Node: "alpha", Path: "/etc/app.conf", Content: "x"},
		{Node: "alpha", Path: "etc/app.conf", Content: "x"},
		{Node: "alpha", Path: "/etc/new.conf", Content: strings.Repeat("a", writeLimit+1)},
		{Node: "alpha", Path: "/etc/new.conf", Content: "a\x00b"},
	} {
		if _, err := toolWriteNodeFile(c, bad); err == nil {
			t.Errorf("应该拒绝：%s %d 字节", bad.Path, len(bad.Content))
		}
	}
	// 敏感文件的预览里有提醒。
	v, err = toolWriteNodeFile(c, WriteNodeFileArgs{Node: "alpha", Path: "/etc/shadow", Content: "x", ExpectedSHA256: "abc"})
	if err != nil || v.(map[string]any)["warning"] == nil {
		t.Errorf("敏感文件的预览应该带提醒：%v %v", v, err)
	}
}

func TestDeleteNodeFileConfirm(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	in := NodeConfirmPathArgs{NodePathArgs: NodePathArgs{Node: "alpha", Path: "/srv/data"}}
	v, err := toolDeleteNodeFile(c, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Confirm = "data" // 网页那样填目录名不算：要预览给的确认码
	if v2, _ := toolDeleteNodeFile(c, in); v2.(map[string]any)["preview"] == nil || len(fn.called()) != 0 {
		t.Fatal("确认码不对时不能删除")
	}
	in.Confirm = previewCode(t, v)
	if _, err := toolDeleteNodeFile(c, in); err != nil {
		t.Fatal(err)
	}
	if fn.del == nil || fn.del.GetConfirmName() != "data" {
		t.Fatalf("删除目录要带上目录名：%v", fn.del)
	}
	if _, err := toolDeleteNodeFile(c, NodeConfirmPathArgs{NodePathArgs: NodePathArgs{Node: "alpha", Path: "/srv/missing"}}); err == nil {
		t.Error("不存在的路径应该报错")
	}
}

func TestMoveNodeFilesConfirm(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	in := MoveNodeFilesArgs{Node: "alpha", Paths: []string{"/srv/a"}, Action: "move", DestDir: "/srv/b"}
	v, err := toolMoveNodeFiles(c, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Confirm = previewCode(t, v)
	in.DestDir = "/etc"
	if v, _ := toolMoveNodeFiles(c, in); v.(map[string]any)["preview"] == nil {
		t.Fatal("目标变了，原来的确认码不能用")
	}
	in.DestDir = "/srv/b"
	res, err := toolMoveNodeFiles(c, in)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(fileOpView); !r.Done || fn.move == nil || !fn.move.GetConfirmed() || fn.move.GetDestDir() != "/srv/b" {
		t.Fatalf("移动的结果或参数不对：%+v %v", r, fn.move)
	}
	for _, bad := range []MoveNodeFilesArgs{
		{Node: "alpha", Paths: []string{"/"}, Action: "move", DestDir: "/srv"},
		{Node: "alpha", Paths: []string{"/srv/a"}, Action: "move", DestDir: "srv"},
		{Node: "alpha", Paths: []string{"/srv/a"}, Action: "move", DestDir: "/srv", Conflict: "replace"},
		{Node: "alpha", Paths: []string{"/srv/a", "/srv/b"}, Action: "rename", NewName: "c"},
		{Node: "alpha", Paths: []string{"/srv/a"}, Action: "delete", DestDir: "/srv"},
	} {
		if _, err := toolMoveNodeFiles(c, bad); err == nil {
			t.Errorf("应该拒绝：%+v", bad)
		}
	}
}

// 卷文件的移动：默认同名时什么都不改，返回冲突；选覆盖要先预览。
func TestMoveFilesConflict(t *testing.T) {
	c, _, fp := nodeAdminCall(t)
	in := MoveFilesArgs{ContainerArg: ContainerArg{Project: "shop", Container: "web"}, Volume: ".data", Paths: []string{"a.txt"}, Action: "move", DestDir: "dst"}
	v, err := toolMoveFiles(c, in)
	if err != nil {
		t.Fatal(err)
	}
	if r := v.(fileOpView); r.Done || len(r.Conflicts) != 1 {
		t.Fatalf("应该返回冲突：%+v", r)
	}
	in.Conflict = "overwrite"
	v, err = toolMoveFiles(c, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Confirm = previewCode(t, v)
	if v, err = toolMoveFiles(c, in); err != nil || !v.(fileOpView).Done {
		t.Fatalf("确认后应该覆盖：%v %v", v, err)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if strings.Join(fp.calls, ",") != "MoveVolumeFiles:,MoveVolumeFiles:overwrite" {
		t.Fatalf("调用不对：%v", fp.calls)
	}
}

func TestRemoveAndApproveNodeConfirm(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	if v, err := toolRemoveNode(c, NodeConfirmArgs{Node: "alpha"}); err != nil || v.(map[string]any)["preview"] == nil {
		t.Fatalf("移除节点要先预览：%v %v", v, err)
	}
	if _, err := toolRemoveNode(c, NodeConfirmArgs{Node: "alpha", Confirm: "ALPHA"}); err != nil {
		t.Fatal(err)
	}
	if len(fn.called()) != 0 {
		t.Fatal("确认的名称要原样一致")
	}
	if _, err := toolRemoveNode(c, NodeConfirmArgs{Node: "alpha", Confirm: "alpha"}); err != nil {
		t.Fatal(err)
	}
	// 批准：只有等待批准的节点能批准，预览里带上节点报告的主机名。
	if _, err := toolApproveNode(c, NodeConfirmArgs{Node: "alpha", Confirm: "alpha"}); err == nil {
		t.Error("已经批准的节点不能再批准")
	}
	v, err := toolApproveNode(c, NodeConfirmArgs{Node: "beta"})
	if err != nil || v.(map[string]any)["hostname"] != "beta-host" {
		t.Fatalf("批准前的预览不对：%v %v", v, err)
	}
	if _, err := toolApproveNode(c, NodeConfirmArgs{Node: "beta", Confirm: "beta"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fn.called(), ","); got != "RemoveNode,ApproveNode" {
		t.Fatalf("调用不对：%s", got)
	}
}

func TestIsolateNodeConfirm(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	if v, err := toolIsolateNode(c, IsolateArgs{Node: "alpha"}); err != nil || v.(map[string]any)["preview"] == nil {
		t.Fatalf("隔离要先预览：%v %v", v, err)
	}
	if _, err := toolIsolateNode(c, IsolateArgs{Node: "alpha", Undo: true, Confirm: "alpha"}); err == nil {
		t.Error("没有隔离的节点不能解除隔离")
	}
	if _, err := toolIsolateNode(c, IsolateArgs{Node: "alpha", Confirm: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fn.called(), ","); got != "IsolateNode:alpha" {
		t.Fatalf("调用不对：%s", got)
	}
}

func TestWriteContentLimits(t *testing.T) {
	if _, err := writeContent("a", "YQ=="); err == nil {
		t.Error("content 和 content_base64 只能二选一")
	}
	if _, err := writeContent("a\x00", ""); err == nil {
		t.Error("二进制内容要用 base64")
	}
	if _, err := writeContent("", "not base64!"); err == nil {
		t.Error("不是 base64 应该拒绝")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, writeLimit+1))
	if _, err := writeContent("", big); err == nil {
		t.Error("超过 1 MB 应该拒绝")
	}
	if b, err := writeContent("", base64.StdEncoding.EncodeToString([]byte{0, 1, 2})); err != nil || len(b) != 3 {
		t.Errorf("二进制内容：%v %v", b, err)
	}
}

func TestCleanPaths(t *testing.T) {
	if _, err := cleanPaths(nil); err == nil {
		t.Error("没有路径应该拒绝")
	}
	if _, err := cleanPaths([]string{"/"}); err == nil {
		t.Error("不能操作卷的根目录")
	}
	if _, err := cleanPaths(make([]string, 1001)); err == nil {
		t.Error("一次最多 1000 项")
	}
	if got, err := cleanPaths([]string{"/a/b/"}); err != nil || got[0] != "a/b" {
		t.Errorf("cleanPaths = %v %v", got, err)
	}
}
