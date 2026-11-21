package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	idNginx  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	idOld    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	idBuilt  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	idPg     = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	idRedis  = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	idLink   = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	idUnused = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	idOther  = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
	digestPg = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestRd = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// testImageState 是一个节点：nginx 被正在运行的 Nagi 容器使用；旧版 nginx 没人用；本地构建的镜像是已停止容器部署的版本
// （按镜像 ID 记）；postgres 是热备提前拉的（按摘要记）；redis 是备份节点恢复要用的；代理镜像是 Nagi 自己的；
// 还有一个没人用的镜像和一个被别的（非 Nagi）容器用的镜像。
func testImageState() imageState {
	now := time.Now()
	return imageState{
		images: []docker.Image{
			{ID: idNginx, Tags: []string{"nginx:1.27"}, Digests: []string{"nginx@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}, Size: 200, SharedSize: 50, Created: now},
			{ID: idOld, Tags: nil, Digests: []string{"nginx@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}, Size: 190, SharedSize: 40, Created: now},
			{ID: idBuilt, Size: 50, Created: now},
			{ID: idPg, Digests: []string{"postgres@" + digestPg}, Size: 400, Created: now},
			{ID: idRedis, Tags: []string{"redis:7"}, Digests: []string{"redis@" + digestRd}, Size: 100, Created: now},
			{ID: idLink, Tags: []string{"nagi-link:empty"}, Size: 1, Created: now},
			{ID: idUnused, Tags: []string{"busybox:1.36", "busybox:latest"}, Size: 5, SharedSize: -1, Created: now},
			{ID: idOther, Tags: []string{"alpine:3.19"}, Size: 8, Created: now},
		},
		containers: []docker.ImageContainer{
			{Name: "nagi-p1-web-web-1", ImageID: idNginx, Running: true, Labels: map[string]string{"nagi.project": "p1", "nagi.container": "web", "com.docker.compose.service": "web"}},
			{Name: "nagi-link-x", ImageID: idLink, Running: true, Labels: map[string]string{"nagi.link": "x"}},
			{Name: "hand-made", ImageID: idOther, Running: false},
		},
		held: []heldRef{
			{ref: docker.LinkImage, hold: &nagiv1.ImageHold{Kind: holdSystem}},
			{ref: "nginx:1.27", hold: &nagiv1.ImageHold{Kind: holdDeployed, ProjectId: "p1", Container: "web"}},
			{ref: idBuilt, hold: &nagiv1.ImageHold{Kind: holdDeployed, ProjectId: "p1", Container: "api"}},
			{ref: "docker.io/library/postgres:16@" + digestPg, hold: &nagiv1.ImageHold{Kind: holdStandby, ProjectId: "p2", Container: "db"}},
			{ref: "redis@" + digestRd, hold: &nagiv1.ImageHold{Kind: holdBackup, ProjectId: "p3", Container: "cache"}},
		},
		names: map[string]string{"p1": "shop"},
	}
}

func imageByID(list *nagiv1.NodeImages, id string) *nagiv1.NodeImage {
	for _, ni := range list.GetImages() {
		if ni.GetId() == id {
			return ni
		}
	}
	return nil
}

func TestComputeImagesProtects(t *testing.T) {
	list := computeImages(testImageState())
	// 只有没人用、Nagi 也不用的才进清理名单：旧版 nginx 和 busybox。
	want := []string{idOld, idUnused}
	got := slices.Clone(list.GetPruneIds())
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("清理名单 = %v，应为 %v", got, want)
	}
	// 至少能释放的空间按独占的层算：旧版 nginx 190-40，busybox 算不出共用部分时按整个大小。
	if list.GetPruneBytes() != 150+5 {
		t.Errorf("可释放 = %d，应为 155", list.GetPruneBytes())
	}
	for id, kind := range map[string]string{idBuilt: holdDeployed, idPg: holdStandby, idRedis: holdBackup, idLink: holdSystem, idNginx: holdDeployed} {
		ni := imageByID(list, id)
		if len(ni.GetHolds()) == 0 || ni.GetHolds()[0].GetKind() != kind {
			t.Errorf("%s 的原因 = %v，应为 %s", id[:12], ni.GetHolds(), kind)
		}
	}
	web := imageByID(list, idNginx).GetUsers()
	if len(web) != 1 || web[0].GetProjectName() != "shop" || web[0].GetContainer() != "web" || !web[0].GetRunning() {
		t.Errorf("nginx 的使用者 = %v", web)
	}
	if u := imageByID(list, idLink).GetUsers(); len(u) != 1 || !u[0].GetSystem() {
		t.Errorf("代理镜像应标为 Nagi 自己的容器：%v", u)
	}
	if u := imageByID(list, idOther).GetUsers(); len(u) != 1 || u[0].GetProjectId() != "" || u[0].GetRunning() {
		t.Errorf("别的容器（已停止）也算使用：%v", u)
	}
	// 按大小从大到小。
	if list.GetImages()[0].GetId() != idPg {
		t.Errorf("第一项应是最大的镜像")
	}
	// 清理前的提示：旧版 nginx 和正在用的 nginx 是同一个仓库，可能是回滚要用的；没有仓库摘要的是本地构建的。
	if !imageByID(list, idOld).GetPreviousVersion() || imageByID(list, idUnused).GetPreviousVersion() || imageByID(list, idNginx).GetPreviousVersion() {
		t.Error("只有可清理、和 Nagi 容器的镜像同一个仓库的才标为可能的回滚版本")
	}
	if !imageByID(list, idBuilt).GetLocalBuild() || imageByID(list, idOld).GetLocalBuild() || imageByID(list, idPg).GetLocalBuild() {
		t.Error("没有仓库摘要的才标为本地构建")
	}
}

func TestImageMatches(t *testing.T) {
	im := docker.Image{ID: idPg, Tags: []string{"postgres:16"}, Digests: []string{"postgres@" + digestPg}}
	for ref, want := range map[string]bool{
		"postgres:16":                           true,
		"docker.io/library/postgres:16":         true,
		"postgres":                              false, // 是 latest
		"postgres@" + digestPg:                  true,
		"postgres:15@" + digestPg:               true, // 带摘要时以摘要为准
		"mysql@" + digestPg:                     false,
		"postgres@" + digestRd:                  false,
		idPg:                                    true,
		idPg[:19]:                               false,
		"":                                      false,
		"not a ref":                             false,
		"other@" + strings.TrimPrefix(idPg, ""): true, // containerd 镜像存储：镜像 ID 就是摘要
	} {
		if got := imageMatches(im, ref); got != want {
			t.Errorf("imageMatches(%q) = %v，应为 %v", ref, got, want)
		}
	}
}

func TestSpecRefs(t *testing.T) {
	s := &nagiv1.ContainerSpec{ProjectId: "p1", ProjectName: "shop", Name: "web",
		Images: map[string]string{"web": "nginx@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		Config: &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "nginx:1.27"}}}
	refs := specRefs(s, holdStandby)
	if len(refs) != 2 || refs[0].hold.GetKind() != holdStandby || refs[0].hold.GetContainer() != "web" || refs[1].ref != "nginx:1.27" {
		t.Fatalf("specRefs = %+v", refs)
	}
}

// fakeRemover 模拟 Docker：记下删除的引用，按引用找到镜像并去掉，最后一个引用去掉时镜像删除。
type fakeRemover struct {
	refs    map[string]string // 引用 → 镜像 ID
	removed []string
	fail    map[string]error
}

func (f *fakeRemover) RemoveImage(_ context.Context, ref string) (bool, error) {
	if err := f.fail[ref]; err != nil {
		return false, err
	}
	id, ok := f.refs[ref]
	if !ok {
		return false, errors.New("No such image: " + ref)
	}
	f.removed = append(f.removed, ref)
	delete(f.refs, ref)
	if ref == id {
		for r, x := range f.refs {
			if x == id {
				return false, errors.New("conflict: image is referenced in multiple repositories")
			}
			_ = r
		}
		return true, nil
	}
	for _, x := range f.refs {
		if x == id {
			return false, nil
		}
	}
	return true, nil
}

func (f *fakeRemover) ImageID(_ context.Context, ref string) (string, error) {
	for r, id := range f.refs {
		if r == ref || id == ref {
			return id, nil
		}
	}
	return "", errors.New("No such image")
}

func newFakeRemover() *fakeRemover {
	return &fakeRemover{refs: map[string]string{
		"nginx:1.27": idNginx, idNginx: idNginx,
		"busybox:1.36": idUnused, "busybox:latest": idUnused, idUnused: idUnused,
		"nginx@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd": idOld, idOld: idOld,
		"redis:7": idRedis, idRedis: idRedis, "alpine:3.19": idOther, idOther: idOther, idPg: idPg, idBuilt: idBuilt, idLink: idLink,
	}}
}

func TestDeleteImagesRefuses(t *testing.T) {
	list := computeImages(testImageState())
	rm := newFakeRemover()
	res := deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{
		{Id: idNginx},                     // 正在被 Nagi 容器使用
		{Id: idOther},                     // 被别的已停止容器使用
		{Id: idPg},                        // 热备要用
		{Id: idRedis, Tag: "redis:7"},     // 备份节点要用，只删标签也不行
		{Id: idBuilt},                     // 已停止容器部署的版本
		{Id: idLink},                      // Nagi 自己的
		{Id: idUnused, Tag: "nginx:1.27"}, // 标签不属于这个镜像
		{Id: "sha256:" + strings.Repeat("9", 64)}, // 不存在
	})
	for _, r := range res {
		if r.GetError() == "" {
			t.Errorf("%s %s 应被拒绝", r.GetId()[:12], r.GetTag())
		}
	}
	if len(rm.removed) != 0 {
		t.Fatalf("不应删除任何引用：%v", rm.removed)
	}
	if !strings.Contains(res[1].GetError(), "hand-made") {
		t.Errorf("应写出使用它的容器：%q", res[1].GetError())
	}
}

func TestDeleteImagesTagAndWhole(t *testing.T) {
	list := computeImages(testImageState())
	rm := newFakeRemover()
	res := deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{{Id: idUnused, Tag: "busybox:latest"}})
	if res[0].GetError() != "" || !slices.Equal(rm.removed, []string{"busybox:latest"}) {
		t.Fatalf("只删标签：%v %v", res[0].GetError(), rm.removed)
	}
	if _, ok := rm.refs[idUnused]; !ok {
		t.Fatal("只删一个标签时镜像应保留")
	}
	rm = newFakeRemover()
	res = deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{{Id: idUnused}, {Id: idOld}})
	for _, r := range res {
		if r.GetError() != "" {
			t.Fatalf("删除整个镜像失败：%s", r.GetError())
		}
	}
	if _, err := rm.ImageID(context.Background(), idUnused); err == nil {
		t.Error("busybox 应已删除")
	}
	if _, err := rm.ImageID(context.Background(), idOld); err == nil {
		t.Error("没有标签的旧镜像应已删除")
	}
}

// 按 ID 删整个镜像时带上预览时的标签（审查 L5）：之后新加的标签不跟着删，跳过并报告。
func TestDeleteWholeImageNewTag(t *testing.T) {
	list := computeImages(testImageState())
	rm := newFakeRemover()
	// 预览时 busybox 只有 1.36，确认前多了 latest。
	res := deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{{Id: idUnused, Tags: []string{"busybox:1.36"}, CheckTags: true}})
	if !strings.Contains(res[0].GetError(), "busybox:latest") || len(rm.removed) != 0 {
		t.Fatalf("多了标签应跳过：%q %v", res[0].GetError(), rm.removed)
	}
	// 预览时没有标签的镜像，现在有了也一样。
	res = deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{{Id: idUnused, CheckTags: true}})
	if res[0].GetError() == "" || len(rm.removed) != 0 {
		t.Fatalf("预览时没有标签、现在有了应跳过：%q %v", res[0].GetError(), rm.removed)
	}
	// 标签没变：照常删除。
	res = deleteImages(context.Background(), rm, list, []*nagiv1.ImageRef{{Id: idUnused, Tags: []string{"busybox:latest", "busybox:1.36"}, CheckTags: true}})
	if res[0].GetError() != "" {
		t.Fatalf("标签没变应删除：%s", res[0].GetError())
	}
	if _, err := rm.ImageID(context.Background(), idUnused); err == nil {
		t.Error("busybox 应已删除")
	}
}

func TestPruneOnlyCandidates(t *testing.T) {
	list := computeImages(testImageState())
	rm := newFakeRemover()
	// 被攻破的面板塞进受保护的镜像：节点只删现在仍然可以清理的。
	out := pruneImages(context.Background(), rm, list, []string{idOld, idPg, idNginx, idLink, idUnused, idOld})
	if out.GetDeleted() != 2 || out.GetSkipped() != 4 || len(out.GetFailed()) != 0 {
		t.Fatalf("清理结果 = %+v", out)
	}
	if !slices.Equal(out.GetDeletedIds(), []string{idOld, idUnused}) {
		t.Errorf("实际删除的 ID 要列出来（审计用）：%v", out.GetDeletedIds())
	}
	for _, id := range []string{idPg, idNginx, idLink, idRedis, idBuilt, idOther} {
		if _, err := rm.ImageID(context.Background(), id); err != nil {
			t.Errorf("%s 不应被清理", id[:12])
		}
	}
	// 预览之后新出现的可清理镜像不在请求里，也不删。
	rm = newFakeRemover()
	out = pruneImages(context.Background(), rm, list, []string{idOld})
	if out.GetDeleted() != 1 {
		t.Fatalf("清理结果 = %+v", out)
	}
	if _, err := rm.ImageID(context.Background(), idUnused); err != nil {
		t.Error("没有列在请求里的镜像不应被清理")
	}
}

func TestNormalizeImageRef(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                  "docker.io/library/nginx:latest",
		" nginx:1.27 ":           "docker.io/library/nginx:1.27",
		"ghcr.io/owner/app:v1":   "ghcr.io/owner/app:v1",
		"registry:5000/team/app": "registry:5000/team/app:latest",
		"nginx@" + digestPg:      "docker.io/library/nginx@" + digestPg,
		"nginx:1.27@" + digestPg: "docker.io/library/nginx:1.27@" + digestPg,
	} {
		got, _, err := normalizeImageRef(in)
		if err != nil || got != want {
			t.Errorf("normalizeImageRef(%q) = %q, %v，应为 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Nginx", "nginx:", "-nginx", "nginx latest", "a\nb", strings.Repeat("a", 64), idPg, "nginx@sha256:abc", strings.Repeat("a/", 300) + "b"} {
		if _, _, err := normalizeImageRef(in); status.Code(err) != codes.InvalidArgument {
			t.Errorf("normalizeImageRef(%q) 应拒绝，得到 %v", in, err)
		}
	}
	if _, domain, _ := normalizeImageRef("ghcr.io/owner/app"); domain != "ghcr.io" {
		t.Errorf("域名 = %q", domain)
	}
}

// 每个节点同时最多 maxPulls 个拉取，多出来的直接拒绝。
func TestPullImageConcurrencyLimit(t *testing.T) {
	for range maxPulls {
		pullSlots <- struct{}{}
	}
	defer func() {
		for range maxPulls {
			<-pullSlots
		}
	}()
	s := &agentService{node: &agent{}}
	err := s.PullImage(&nagiv1.PullImageRequest{Image: "nginx:1.27"}, nil)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("名额用完时应拒绝：%v", err)
	}
}

// 拉取过程中磁盘剩余空间不足时停下，原因是磁盘不足。
func TestWatchPullDisk(t *testing.T) {
	prev := pullDiskInterval
	pullDiskInterval = 10 * time.Millisecond
	defer func() { pullDiskInterval = prev }()
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	low := &metrics.LowDiskError{Total: 100 << 30, Free: 1 << 30}
	calls := 0
	go watchPullDisk(ctx, func() error {
		if calls++; calls >= 3 {
			return low
		}
		return nil
	}, stop)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("磁盘不足时应停止拉取")
	}
	var got *metrics.LowDiskError
	if !errors.As(context.Cause(ctx), &got) {
		t.Fatalf("原因应是磁盘不足：%v", context.Cause(ctx))
	}
}

// 拉取镜像跟着调用方取消（用户点取消、关掉页面时停止），其余操作开始了就做完。
func TestPullImageFollowsCaller(t *testing.T) {
	if !isFollowStream(nagiv1.AgentService_PullImage_FullMethodName) {
		t.Error("PullImage 应在调用方取消时停止")
	}
	if isFollowStream(nagiv1.AgentService_DeployContainer_FullMethodName) {
		t.Error("部署不应跟着调用方取消")
	}
	if !readOnlyRPCs[nagiv1.AgentService_ListImages_FullMethodName] || readOnlyRPCs[nagiv1.AgentService_DeleteImages_FullMethodName] || readOnlyRPCs[nagiv1.AgentService_PullImage_FullMethodName] {
		t.Error("备用面板只能列出镜像")
	}
}

type imageHistoryStream struct {
	grpc.ServerStream
	ctx     context.Context
	sendErr error
}

func (s *imageHistoryStream) Context() context.Context          { return s.ctx }
func (s *imageHistoryStream) Send(*nagiv1.ImagePullEvent) error { return s.sendErr }

func TestImagePullHistory(t *testing.T) {
	for _, mode := range []string{"ok", "daemon_failure", "viewer_gone"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/info"):
					io.WriteString(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/images/create"):
					io.WriteString(w, "{\"id\":\"layer\",\"status\":\"Downloading\",\"progressDetail\":{\"current\":17,\"total\":91}}\n")
					if mode == "daemon_failure" {
						io.WriteString(w, "{\"errorDetail\":{\"message\":\"registry unavailable\"}}\n")
					}
				case strings.HasSuffix(r.URL.Path, "/json"):
					io.WriteString(w, `{"Id":"`+idNginx+`"}`)
				default:
					t.Errorf("unexpected Docker endpoint: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.56")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			dc, err := docker.New()
			if err != nil {
				t.Fatal(err)
			}
			defer dc.Close()
			path := filepath.Join(t.TempDir(), "logs.db")
			journal, err := logstore.Open(t.Context(), path, 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			a := &agent{docker: dc, cfg: Config{Logs: journal}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			stream := &imageHistoryStream{ctx: t.Context()}
			if mode == "viewer_gone" {
				stream.sendErr = io.ErrClosedPipe
			}
			err = (&agentService{node: a}).PullImage(&nagiv1.PullImageRequest{Image: "nginx:1.27"}, stream)
			if mode == "ok" && err != nil || mode == "daemon_failure" && status.Code(err) != codes.FailedPrecondition || mode == "viewer_gone" && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("pull result changed: %v", err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			journal, err = logstore.Open(t.Context(), path, 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			rows, err := journal.Query(t.Context(), logstore.Filter{Category: logstore.Task})
			if err != nil || len(rows) != 3 {
				t.Fatalf("history: %+v %v", rows, err)
			}
			var taskID string
			for i, row := range rows {
				var data struct {
					TaskID               string `json:"task_id"`
					Phase, Result, Error string
					Pulled               bool
					Current, Total       int64
				}
				if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					taskID = data.TaskID
				}
				if taskID == "" || data.TaskID != taskID || row.Kind != "image.pull" || row.Object != "docker.io/library/nginx:1.27" || data.Phase != []string{"started", "progress", "finished"}[i] {
					t.Fatalf("task association: %+v %s", row, row.Data)
				}
				if i == 1 && (data.Current != 17 || data.Total != 91) {
					t.Fatalf("progress: %s", row.Data)
				}
				if i == 2 && (data.Pulled != (mode != "daemon_failure") || (data.Result == "ok") != (mode == "ok") || (data.Error != "") != (mode != "ok")) {
					t.Fatalf("completion: %s", row.Data)
				}
			}
		})
	}
}

func TestImageDeletionHistoryPreservesResultsAndGuards(t *testing.T) {
	for _, mode := range []string{"delete", "prune", "cache_failure", "scan_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "logs.db")
			journal, err := logstore.Open(ctx, path, 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { journal.Close() }()
			var removed []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodDelete:
					if strings.HasSuffix(r.URL.Path, "/images/bad:1") {
						// The preceding image must already have its own durable
						// result; don't wait for the whole batch to finish.
						rows, err := journal.Query(ctx, logstore.Filter{Object: idUnused})
						if err != nil || len(rows) != 1 {
							t.Errorf("previous decision missing before next deletion: %+v %v", rows, err)
						}
						w.WriteHeader(http.StatusConflict)
						io.WriteString(w, `{"message":"safe-db --password Sensitive123"}`)
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/images/good:1") || r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true" {
						t.Errorf("unsafe or unexpected deletion: %s", r.URL)
					}
					removed = append(removed, r.URL.Path)
					io.WriteString(w, `[{"Deleted":"`+idUnused+`"}]`)
				case strings.HasSuffix(r.URL.Path, "/images/json"):
					if mode == "scan_failure" {
						w.WriteHeader(http.StatusInternalServerError)
						io.WriteString(w, `{"message":"daemon unavailable"}`)
						return
					}
					io.WriteString(w, `[{"Id":"`+idUnused+`","RepoTags":["good:1"],"Size":17},`+
						`{"Id":"`+idOld+`","RepoTags":["bad:1"],"Size":91},`+
						`{"Id":"`+idOther+`","RepoTags":["protected:1"],"Size":123}]`)
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					io.WriteString(w, `[{"Id":"c","Names":["/protected"],"ImageID":"`+idOther+`","State":"running"}]`)
				case strings.HasSuffix(r.URL.Path, "/build/prune"):
					if mode == "cache_failure" {
						w.WriteHeader(http.StatusInternalServerError)
						io.WriteString(w, `{"message":"cache unavailable"}`)
						return
					}
					io.WriteString(w, `{"SpaceReclaimed":37}`)
				default:
					t.Errorf("unexpected Docker endpoint: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.56")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			dc, err := docker.New()
			if err != nil {
				t.Fatal(err)
			}
			defer dc.Close()
			d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			projects, err := project.NewManager(d, dc, project.DefaultSubnetPool, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := &agent{docker: dc, projects: projects, replicas: newReplicas(t.TempDir()), cfg: Config{Logs: journal},
				log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			s := &agentService{node: a}
			kind, wantItems, wantCache := "image.prune", 4, 1
			if mode == "delete" || mode == "scan_failure" {
				kind, wantItems, wantCache = "image.delete", 3, 0
				out, err := s.DeleteImages(ctx, &nagiv1.DeleteImagesRequest{Images: []*nagiv1.ImageRef{{Id: idUnused, Tag: "good:1"}, {Id: idOld, Tag: "bad:1"}, {Id: idOther}}})
				if mode == "scan_failure" {
					wantItems = 0
					if status.Code(err) != codes.Unavailable {
						t.Fatalf("scan failure changed: %+v %v", out, err)
					}
				} else if err != nil || len(out.GetResults()) != 3 || out.Results[0].GetError() != "" || out.Results[1].GetError() == "" || out.Results[2].GetError() == "" {
					t.Fatalf("delete guards/results changed: %+v %v", out, err)
				}
			} else {
				out, err := s.PruneImages(ctx, &nagiv1.PruneImagesRequest{Ids: []string{idUnused, idOld, idOther, idUnused}, BuildCache: true})
				wantFailed, wantFreed := 1, int64(37)
				if mode == "cache_failure" {
					wantFailed, wantFreed = 2, 0
				}
				if err != nil || out.GetDeleted() != 1 || out.GetSkipped() != 2 || len(out.GetFailed()) != wantFailed || out.GetBuildCacheFreed() != wantFreed || !slices.Equal(out.GetDeletedIds(), []string{idUnused}) {
					t.Fatalf("prune results changed: %+v %v", out, err)
				}
			}
			if mode != "scan_failure" && len(removed) != 1 || mode == "scan_failure" && len(removed) != 0 {
				t.Fatalf("duplicate or protected image deleted: %v", removed)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			journal, err = logstore.Open(ctx, path, 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := journal.Query(ctx, logstore.Filter{Category: logstore.Task})
			if err != nil || len(rows) != 2+wantItems+wantCache {
				t.Fatalf("persistent lifecycle: %+v %v", rows, err)
			}
			var taskID string
			item := 0
			for i, row := range rows {
				var data struct {
					TaskID               string `json:"task_id"`
					Phase, Result, Error string
					FreedBytes           int64 `json:"freed_bytes"`
				}
				if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					taskID = data.TaskID
					if data.Phase != "started" {
						t.Fatal("missing task start")
					}
				}
				if taskID == "" || data.TaskID != taskID || row.Kind != kind || strings.Contains(string(row.Data.(json.RawMessage)), "Sensitive123") {
					t.Fatalf("task correlation or redaction: %+v", row)
				}
				if data.Phase == "item" {
					want := []string{"ok", "failed", "failed"}
					if mode != "delete" {
						want = []string{"ok", "failed", "skipped", "skipped"}
					}
					if data.Result != want[item] {
						t.Fatalf("item result: %+v", data)
					}
					item++
				}
				if data.Phase == "build_cache" && mode == "prune" && (data.FreedBytes != 37 || data.Result != "ok") {
					t.Fatalf("cache result: %+v", data)
				}
				if i == len(rows)-1 {
					want := "partial"
					if mode == "scan_failure" {
						want = "failed"
					}
					if data.Phase != "finished" || data.Result != want {
						t.Fatalf("failed items became success: %+v", data)
					}
				}
			}
		})
	}
}
