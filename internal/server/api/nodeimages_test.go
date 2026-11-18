package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 旧版本的节点没有镜像接口：面板提示升级节点（界面按 NodeInfo.image_ops 隐藏入口）。
func TestNodeImagesOldNode(t *testing.T) {
	err := nodeAccessError(status.Error(codes.Unimplemented, "unknown method ListImages"))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("应返回 FailedPrecondition：%v", err)
	}
	if got := ce.Message(); got != "节点版本过低，升级后才能使用此功能" {
		t.Errorf("提示 = %q", got)
	}
}

// 节点返回的每一项错误按请求的语言显示。
func TestLocalizeImageResults(t *testing.T) {
	list := []*nagiv1.ImageDeleteResult{{Error: "镜像正在被容器 web-1 使用"}, {Error: "Nagi 需要这个镜像，不能删除"}, {}}
	localizeImageResults(i18n.WithLang(context.Background(), i18n.En), list)
	if list[0].GetError() != "The image is used by container web-1" || list[1].GetError() != "Nagi needs this image, so it can't be deleted" || list[2].GetError() != "" {
		t.Errorf("翻译 = %v", list)
	}
}

// 删除、清理镜像要先做高权限确认；访问令牌改由 MCP 工具的预览和确认码代替。拉取和查看不用确认。
func TestNodeImagesElevation(t *testing.T) {
	del, prune := nagiv1connect.NodeServiceDeleteNodeImagesProcedure, nagiv1connect.NodeServicePruneNodeImagesProcedure
	web := auth.User{ID: 1, Role: auth.RoleAdmin}
	token := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 3, AllowWrite: true}
	for _, p := range []string{del, prune} {
		if !needsElevation(web, p) {
			t.Errorf("网页会话调用 %s 应先做高权限确认", p)
		}
		if needsElevation(token, p) {
			t.Errorf("令牌调用 %s 由 MCP 的确认码代替高权限确认", p)
		}
	}
	for _, p := range []string{nagiv1connect.NodeServicePullNodeImageProcedure, nagiv1connect.NodeServiceListNodeImagesProcedure} {
		if needsElevation(web, p) {
			t.Errorf("%s 不需要高权限确认", p)
		}
	}
	// 令牌的例外只限镜像：节点文件照旧要确认（令牌做不了，所以用不了）。
	if !needsElevation(token, nagiv1connect.NodeServiceListNodeDirProcedure) {
		t.Error("令牌不能绕过节点文件的高权限确认")
	}

	// 实际的网页会话：没有确认时拦截器拒绝，带上要确认的标记。
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
	_, err := nodes.DeleteNodeImages(ctx, connect.NewRequest(&nagiv1.DeleteNodeImagesRequest{NodeId: "n1", Images: []*nagiv1.ImageRef{{Id: "sha256:x"}}}))
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get(elevationHeader) != "required" {
		t.Fatalf("没有高权限确认时应拒绝：%v", err)
	}
	_, err = nodes.PruneNodeImages(ctx, connect.NewRequest(&nagiv1.PruneNodeImagesRequest{NodeId: "n1", Ids: []string{"sha256:x"}}))
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Meta().Get(elevationHeader) != "required" {
		t.Fatalf("清理没有高权限确认时应拒绝：%v", err)
	}
}

// fakeImages 模拟节点镜像接口：按请求的 ID 返回结果，ID 以 bad 开头的失败。
type fakeImages struct {
	nagiv1connect.UnimplementedNodeServiceHandler
}

func (fakeImages) DeleteNodeImages(_ context.Context, req *connect.Request[nagiv1.DeleteNodeImagesRequest]) (*connect.Response[nagiv1.DeleteImagesResponse], error) {
	out := &nagiv1.DeleteImagesResponse{}
	for _, r := range req.Msg.GetImages() {
		res := &nagiv1.ImageDeleteResult{Id: r.GetId(), Tag: r.GetTag()}
		if strings.HasPrefix(r.GetId(), "sha256:bad") {
			res.Error = "镜像正在被容器 web-1 使用"
		}
		out.Results = append(out.Results, res)
	}
	return connect.NewResponse(out), nil
}

func (fakeImages) PruneNodeImages(_ context.Context, req *connect.Request[nagiv1.PruneNodeImagesRequest]) (*connect.Response[nagiv1.PruneImagesResponse], error) {
	out := &nagiv1.PruneImagesResponse{}
	for _, id := range req.Msg.GetIds() {
		if strings.HasPrefix(id, "sha256:bad") {
			out.Failed = append(out.Failed, &nagiv1.ImageDeleteResult{Id: id, Error: "删除失败：conflict"})
		} else if strings.HasPrefix(id, "sha256:gone") {
			out.Skipped++
		} else {
			out.Deleted++
			out.DeletedIds = append(out.DeletedIds, id)
		}
	}
	if req.Msg.GetBuildCache() {
		out.BuildCacheFreed = 100
	}
	return connect.NewResponse(out), nil
}

// 批量删除、清理：每一项都失败时记为失败，部分成功仍记为成功，参数里列出失败的几项。
func TestNodeImagesAuditBatch(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	_, handler := nagiv1connect.NewNodeServiceHandler(fakeImages{}, connect.WithInterceptors(activityLog{h.a}))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := nagiv1connect.NewNodeServiceClient(srv.Client(), srv.URL)
	type row struct{ action, result, errText, params string }
	last := func() row {
		var r row
		if err := h.a.Audit.DB().QueryRow(`SELECT action, result, error, params FROM events ORDER BY id DESC LIMIT 1`).Scan(&r.action, &r.result, &r.errText, &r.params); err != nil {
			t.Fatal(err)
		}
		return r
	}
	del := func(ids ...string) {
		var refs []*nagiv1.ImageRef
		for _, id := range ids {
			refs = append(refs, &nagiv1.ImageRef{Id: id})
		}
		if _, err := client.DeleteNodeImages(ctx, connect.NewRequest(&nagiv1.DeleteNodeImagesRequest{NodeId: "n1", Images: refs})); err != nil {
			t.Fatal(err)
		}
	}

	del("sha256:good1")
	if r := last(); r.result != "ok" || strings.Contains(r.params, `"failed":`) || !strings.Contains(r.params, `"deleted":1`) {
		t.Errorf("全部成功：%+v", r)
	}
	del("sha256:good1", "sha256:bad222222222")
	if r := last(); r.result != "ok" || !strings.Contains(r.params, `"failed"`) || !strings.Contains(r.params, "bad222222222") || !strings.Contains(r.params, "web-1") {
		t.Errorf("部分成功应记为成功，并列出失败的一项：%+v", r)
	}
	del("sha256:bad1", "sha256:bad2")
	if r := last(); r.action != "DeleteNodeImages" || r.result != "failed" || r.errText == "" {
		t.Errorf("全部失败应记为失败：%+v", r)
	}

	prune := func(cache bool, ids ...string) {
		if _, err := client.PruneNodeImages(ctx, connect.NewRequest(&nagiv1.PruneNodeImagesRequest{NodeId: "n1", Ids: ids, BuildCache: cache})); err != nil {
			t.Fatal(err)
		}
	}
	prune(false, "sha256:bad1")
	if r := last(); r.action != "PruneNodeImages" || r.result != "failed" {
		t.Errorf("清理全部失败应记为失败：%+v", r)
	}
	// 镜像都没删掉，但构建缓存清理了：算部分成功。
	prune(true, "sha256:bad1")
	if r := last(); r.result != "ok" || !strings.Contains(r.params, "failed") {
		t.Errorf("构建缓存清理成功时应记为成功：%+v", r)
	}
	prune(false, "sha256:good", "sha256:bad1")
	if r := last(); r.result != "ok" {
		t.Errorf("清理部分成功应记为成功：%+v", r)
	}

	// 一次删除、清理几百个（审查 L4）：参数不超过上限，数量和实际删除的前几项都在记录里。
	var ids []string
	for i := range 300 {
		ids = append(ids, fmt.Sprintf("sha256:%064x", i+1))
	}
	ids = append(ids, "sha256:bad"+strings.Repeat("0", 61), "sha256:gone"+strings.Repeat("0", 60))
	type summary struct {
		Requested    int      `json:"requested"`
		Deleted      int      `json:"deleted"`
		Skipped      int      `json:"skipped"`
		FailedCount  int      `json:"failed_count"`
		DeletedItems []string `json:"deleted_items"`
		Failed       []string `json:"failed"`
	}
	check := func(what string, wantSkipped int) {
		t.Helper()
		r := last()
		if len(r.params) > audit.MaxParams {
			t.Fatalf("%s：参数超过上限 %d", what, len(r.params))
		}
		var s summary
		if err := json.Unmarshal([]byte(r.params), &s); err != nil {
			t.Fatalf("%s：参数应该是完整的 JSON：%v %s", what, err, r.params)
		}
		wantDeleted := 300
		if wantSkipped == 0 {
			wantDeleted = 301 // 删除时 gone 也算成功
		}
		if s.Requested != 302 || s.Deleted != wantDeleted || s.FailedCount != 1 || s.Skipped != wantSkipped {
			t.Errorf("%s：数量不对：%+v", what, s)
		}
		if len(s.DeletedItems) != maxBatchListed || s.DeletedItems[0] != strings.TrimPrefix(ids[0], "sha256:")[:12] {
			t.Errorf("%s：应该列出实际删除的前 %d 项：%v", what, maxBatchListed, s.DeletedItems)
		}
		if len(s.Failed) != 1 {
			t.Errorf("%s：应该列出失败的一项：%v", what, s.Failed)
		}
	}
	del(ids...)
	check("删除", 0)
	prune(false, ids...)
	check("清理", 1)
}
