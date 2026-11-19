package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 节点镜像的工具只给管理员；删除、清理、拉取要令牌勾了「可修改」，备用面板上没有。
func TestNodeImageToolsAllowed(t *testing.T) {
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	read := auth.User{Role: auth.RoleAdmin, TokenID: 1}
	write := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true}
	operator := auth.User{Role: auth.RoleOperator, Perms: auth.PresetOperator, TokenID: 1, AllowWrite: true, AllowExec: true}
	for _, name := range []string{"list_node_images", "delete_node_images", "prune_node_images", "pull_node_image"} {
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("没有工具 %s", name)
		}
		isWrite := name != "list_node_images"
		if got := tl.allowedFor(read, false); got != !isWrite {
			t.Errorf("%s 对只读令牌 = %v", name, got)
		}
		if !tl.allowedFor(write, false) {
			t.Errorf("%s 对可修改的管理员令牌应可用", name)
		}
		if tl.allowedFor(operator, false) {
			t.Errorf("%s 不应给操作员", name)
		}
		if got := tl.allowedFor(write, true); got != !isWrite {
			t.Errorf("%s 在备用面板上 = %v", name, got)
		}
		if isWrite && !tl.write {
			t.Errorf("%s 应声明为修改", name)
		}
	}
	for _, name := range []string{"delete_node_images", "prune_node_images"} {
		if !byName[name].destructive {
			t.Errorf("%s 应声明为 destructive", name)
		}
	}
}

// fakeNodes 模拟面板的节点接口：一个节点，镜像列表可以在两次调用之间改变。
type fakeNodes struct {
	nagiv1connect.NodeServiceClient
	images  *nagiv1.NodeImages
	deleted []*nagiv1.ImageRef
	pruned  *nagiv1.PruneNodeImagesRequest
}

func (f *fakeNodes) ListNodes(context.Context, *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	return connect.NewResponse(&nagiv1.ListNodesResponse{Nodes: []*nagiv1.Node{{Id: "n1", Name: "hk"}}}), nil
}

func (f *fakeNodes) ListNodeImages(context.Context, *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.NodeImages], error) {
	return connect.NewResponse(f.images), nil
}

func (f *fakeNodes) DeleteNodeImages(_ context.Context, req *connect.Request[nagiv1.DeleteNodeImagesRequest]) (*connect.Response[nagiv1.DeleteImagesResponse], error) {
	f.deleted = append(f.deleted, req.Msg.GetImages()...)
	out := &nagiv1.DeleteImagesResponse{}
	for _, r := range req.Msg.GetImages() {
		out.Results = append(out.Results, &nagiv1.ImageDeleteResult{Id: r.GetId(), Tag: r.GetTag()})
	}
	return connect.NewResponse(out), nil
}

func (f *fakeNodes) PruneNodeImages(_ context.Context, req *connect.Request[nagiv1.PruneNodeImagesRequest]) (*connect.Response[nagiv1.PruneImagesResponse], error) {
	f.pruned = req.Msg
	return connect.NewResponse(&nagiv1.PruneImagesResponse{Deleted: int32(len(req.Msg.GetIds()))}), nil
}

const (
	imgFree  = "sha256:aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	imgTwo   = "sha256:bbbbbbbbbbbb1111111111111111111111111111111111111111111111111111"
	imgInUse = "sha256:cccccccccccc1111111111111111111111111111111111111111111111111111"
	imgNagi  = "sha256:dddddddddddd1111111111111111111111111111111111111111111111111111"
)

func testNodeImages() *nagiv1.NodeImages {
	return &nagiv1.NodeImages{
		Images: []*nagiv1.NodeImage{
			{Id: imgFree, Tags: []string{"busybox:1.36"}, Size: 5 << 20},
			{Id: imgTwo, Tags: []string{"app:a", "app:b"}, Size: 7 << 20},
			{Id: imgInUse, Tags: []string{"nginx:1.27"}, Users: []*nagiv1.ImageUser{{Name: "web-1", Running: true}}},
			{Id: imgNagi, Tags: []string{"postgres:16"}, Holds: []*nagiv1.ImageHold{{Kind: "standby", ProjectName: "shop", Container: "db"}}},
		},
		PruneIds: []string{imgFree, imgTwo}, PruneBytes: 12 << 20, BuildCacheBytes: 30 << 20,
	}
}

func testCall(f *fakeNodes) *call {
	u := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true}
	return &call{ctx: context.Background(), u: u, h: &handler{clients: clients{nodes: f}}}
}

// 删除镜像：先预览（列出会删的和节点会拒绝的），确认码对上才删；只发能删的，不发正在使用和 Nagi 需要的。
func TestDeleteNodeImagesConfirm(t *testing.T) {
	f := &fakeNodes{images: testNodeImages()}
	c := testCall(f)
	args := DeleteNodeImagesArgs{Node: "hk", Images: []string{"busybox:1.36", "nginx:1.27", "dddddddddddd"}, Tags: []string{"app:b"}}
	v, err := toolDeleteNodeImages(c, args)
	if err != nil {
		t.Fatal(err)
	}
	pv := v.(map[string]any)
	code := c.confirmCode("delete", "n1", imgFree+"||busybox:1.36", imgTwo+"|app:b|")
	if next, _ := pv["next"].(string); !strings.Contains(next, code) {
		t.Fatalf("预览里应给出确认码 %s：%v", code, pv)
	}
	if len(f.deleted) != 0 {
		t.Fatal("预览不应删除")
	}
	if refused, _ := pv["refused"].(map[string]string); len(refused) != 2 {
		t.Errorf("正在使用和 Nagi 需要的应列为拒绝：%v", pv["refused"])
	}
	args.Confirm = "000000000000"
	if _, err := toolDeleteNodeImages(c, args); err != nil || len(f.deleted) != 0 {
		t.Fatalf("确认码不对时不应删除：%v %v", err, f.deleted)
	}
	args.Confirm = code
	if _, err := toolDeleteNodeImages(c, args); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range f.deleted {
		got = append(got, r.GetId()+"|"+r.GetTag())
	}
	slices.Sort(got)
	if want := []string{imgFree + "|", imgTwo + "|app:b"}; !slices.Equal(got, want) {
		t.Errorf("删除 = %v，应为 %v", got, want)
	}
	// 删整个镜像时带上预览时的标签，节点据此跳过之后新加了标签的镜像（审查 L5）。
	for _, r := range f.deleted {
		if r.GetTag() == "" && (!r.GetCheckTags() || !slices.Equal(r.GetTags(), []string{"busybox:1.36"})) {
			t.Errorf("删整个镜像应带上预览时的标签：%v", r)
		}
	}
}

// 预览之后镜像多了标签：确认码对不上，不删除（审查 L5）。
func TestDeleteNodeImagesNewTag(t *testing.T) {
	f := &fakeNodes{images: testNodeImages()}
	c := testCall(f)
	args := DeleteNodeImagesArgs{Node: "hk", Images: []string{"busybox:1.36"}}
	v, err := toolDeleteNodeImages(c, args)
	if err != nil {
		t.Fatal(err)
	}
	args.Confirm = previewCode(t, v)
	f.images.Images[0].Tags = append(f.images.Images[0].Tags, "busybox:latest")
	if _, err := toolDeleteNodeImages(c, args); err != nil || len(f.deleted) != 0 {
		t.Fatalf("多了标签以后原来的确认码不能用：%v %v", err, f.deleted)
	}
}

// 清理前提示可能是回滚要用的旧版本、本地构建的镜像，只提示不阻止。
func TestPruneNodeImagesWarnings(t *testing.T) {
	f := &fakeNodes{images: testNodeImages()}
	f.images.Images[0].PreviousVersion = true
	f.images.Images[1].LocalBuild = true
	v, err := toolPruneNodeImages(testCall(f), PruneNodeImagesArgs{Node: "hk"})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := v.(map[string]any)["warnings"].(map[string]any)
	if prev, _ := w["possible_rollback_versions"].([]string); !slices.Equal(prev, []string{"busybox:1.36"}) {
		t.Errorf("应提示可能是回滚要用的旧版本：%v", w)
	}
	if local, _ := w["local_builds"].([]string); !slices.Equal(local, []string{"app:a"}) {
		t.Errorf("应提示本地构建的镜像：%v", w)
	}
}

// 一键清理：预览之后节点上的镜像变了，原来的确认码作废；构建缓存默认不清理。
func TestPruneNodeImagesConfirm(t *testing.T) {
	f := &fakeNodes{images: testNodeImages()}
	c := testCall(f)
	if _, err := toolPruneNodeImages(c, PruneNodeImagesArgs{Node: "hk"}); err != nil || f.pruned != nil {
		t.Fatalf("预览不应清理：%v", err)
	}
	code := c.confirmCode("prune", "n1", "false", imgFree+"|busybox:1.36", imgTwo+"|app:a,app:b")
	// 预览以后多了一个可清理的镜像：确认码对不上，只返回新的预览。
	f.images.PruneIds = append(f.images.PruneIds, imgInUse)
	if _, err := toolPruneNodeImages(c, PruneNodeImagesArgs{Node: "hk", Confirm: code}); err != nil || f.pruned != nil {
		t.Fatalf("镜像变了不应清理：%v", err)
	}
	f.images = testNodeImages()
	if _, err := toolPruneNodeImages(c, PruneNodeImagesArgs{Node: "hk", Confirm: code}); err != nil {
		t.Fatal(err)
	}
	if f.pruned == nil || f.pruned.GetBuildCache() || !slices.Equal(f.pruned.GetIds(), []string{imgFree, imgTwo}) {
		t.Fatalf("清理请求 = %v", f.pruned)
	}
	// 选了构建缓存时确认码不同：不带缓存的确认码不能用来清缓存。
	f.pruned = nil
	if _, err := toolPruneNodeImages(c, PruneNodeImagesArgs{Node: "hk", BuildCache: true, Confirm: code}); err != nil || f.pruned != nil {
		t.Fatalf("构建缓存要单独确认：%v", err)
	}
	if _, err := toolPruneNodeImages(c, PruneNodeImagesArgs{Node: "hk", BuildCache: true, Confirm: c.confirmCode("prune", "n1", "true", imgFree+"|busybox:1.36", imgTwo+"|app:a,app:b")}); err != nil || !f.pruned.GetBuildCache() {
		t.Fatalf("清理构建缓存：%v %v", err, f.pruned)
	}
}
