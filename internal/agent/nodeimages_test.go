package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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
