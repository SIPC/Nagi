package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/distribution/reference"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 节点镜像（见 docs/agent.md#节点镜像）：列出、删除、一键清理、拉取。面板可能被攻破，所以能不能删由节点自己判断：
// 有容器使用的（包括已停止的）、Nagi 要用的（本节点上容器部署的版本、热备和备份节点恢复时要用的、Nagi 自己的）
// 一律不删，也不强制删除。一键清理只删预览时列出、而且现在重新判断仍然可以清理的镜像，不用 docker image prune。

const (
	// pullTimeout 是拉取一个镜像的最长时间。
	pullTimeout = 30 * time.Minute
	// maxImageRef 是镜像引用的最大长度。
	maxImageRef = 512
	// maxImageBatch 是一次最多删除、清理的镜像数。
	maxImageBatch = 1000
)

// 镜像被 Nagi 需要的原因（ImageHold.kind）。
const (
	holdDeployed = "deployed"
	holdStandby  = "standby"
	holdBackup   = "backup"
	holdSystem   = "system"
)

// heldRef 是 Nagi 需要的一个镜像引用和原因。
type heldRef struct {
	ref  string
	hold *nagiv1.ImageHold
}

// imageState 是一次判断用到的全部信息。
type imageState struct {
	images     []docker.Image
	containers []docker.ImageContainer
	held       []heldRef
	names      map[string]string // 项目 ID → 项目名
}

// imageMatches 判断 Nagi 记下的镜像引用（ContainerSpec.images 里的 repo@sha256:...、镜像 ID，或镜像表单里的 repo:tag）
// 是不是指这个本地镜像。
func imageMatches(im docker.Image, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref == im.ID
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	if c, ok := named.(reference.Canonical); ok {
		// containerd 镜像存储里，按摘要拉取的镜像 ID 就是这个摘要。
		if c.Digest().String() == im.ID {
			return true
		}
		for _, d := range im.Digests {
			if dn, err := reference.ParseNormalizedNamed(d); err == nil {
				if dc, ok := dn.(reference.Canonical); ok && dc.Name() == c.Name() && dc.Digest() == c.Digest() {
					return true
				}
			}
		}
		return false
	}
	want := reference.TagNameOnly(named).String()
	for _, t := range im.Tags {
		if tn, err := reference.ParseNormalizedNamed(t); err == nil && reference.TagNameOnly(tn).String() == want {
			return true
		}
	}
	return false
}

// specRefs 返回一个容器配置里 Nagi 需要的镜像：部署时记下的每个镜像（带摘要），以及镜像表单里填的镜像
// （按标签，部署中途刚拉下来、还没记进版本的也算）。
func specRefs(s *nagiv1.ContainerSpec, kind string) []heldRef {
	hold := &nagiv1.ImageHold{Kind: kind, ProjectId: s.GetProjectId(), ProjectName: s.GetProjectName(), Container: s.GetName()}
	var out []heldRef
	svcs := make([]string, 0, len(s.GetImages()))
	for svc := range s.GetImages() {
		svcs = append(svcs, svc)
	}
	sort.Strings(svcs)
	for _, svc := range svcs {
		out = append(out, heldRef{ref: s.GetImages()[svc], hold: hold})
	}
	if img := s.GetConfig().GetForm().GetImage(); img != "" {
		out = append(out, heldRef{ref: img, hold: hold})
	}
	return out
}

// computeImages 按 Docker 的现状和 Nagi 需要的镜像算出列表和一键清理会删除的镜像。
func computeImages(st imageState) *nagiv1.NodeImages {
	users := map[string][]*nagiv1.ImageUser{}
	for _, c := range st.containers {
		u := &nagiv1.ImageUser{Name: c.Name, Running: c.Running}
		if pid := c.Labels[project.LabelProject]; pid != "" {
			u.ProjectId, u.ProjectName, u.Container = pid, st.names[pid], c.Labels["nagi.container"]
			u.Service = c.Labels["com.docker.compose.service"]
		}
		if _, ok := c.Labels[labelLink]; ok {
			u.System = true
		}
		users[c.ImageID] = append(users[c.ImageID], u)
	}
	// Nagi 容器正在用的镜像的仓库：同一个仓库里没人用的镜像，可能是回滚要用的旧版本（只用于清理前提示）。
	repos := map[string]bool{}
	for _, h := range st.held {
		if h.hold.GetKind() == holdSystem {
			continue
		}
		if n, err := reference.ParseNormalizedNamed(strings.TrimSpace(h.ref)); err == nil {
			repos[n.Name()] = true
		}
	}
	out := &nagiv1.NodeImages{}
	for _, im := range st.images {
		ni := &nagiv1.NodeImage{
			Id: im.ID, Tags: im.Tags, Digests: im.Digests, Size: im.Size, UniqueSize: im.Size,
			Created: timestamppb.New(im.Created), Users: users[im.ID],
			// 没有仓库摘要：本地构建或导入的，删除后拉不回来。
			LocalBuild: len(im.Digests) == 0,
		}
		sameRepo := slices.ContainsFunc(append(slices.Clone(im.Tags), im.Digests...), func(r string) bool {
			n, err := reference.ParseNormalizedNamed(r)
			return err == nil && repos[n.Name()]
		})
		if im.SharedSize > 0 {
			ni.UniqueSize = max(0, im.Size-im.SharedSize)
		}
		seen := map[string]bool{}
		for _, h := range st.held {
			if !imageMatches(im, h.ref) {
				continue
			}
			k := h.hold.GetKind() + "/" + h.hold.GetProjectId() + "/" + h.hold.GetContainer()
			if !seen[k] {
				seen[k] = true
				ni.Holds = append(ni.Holds, h.hold)
			}
		}
		if len(ni.Users) == 0 && len(ni.Holds) == 0 {
			out.PruneIds = append(out.PruneIds, im.ID)
			out.PruneBytes += ni.UniqueSize
			ni.PreviousVersion = sameRepo
		}
		out.Images = append(out.Images, ni)
	}
	sort.SliceStable(out.Images, func(i, j int) bool { return out.Images[i].GetSize() > out.Images[j].GetSize() })
	return out
}

// imageState 读出 Docker 的镜像和容器，以及本节点上 Nagi 需要的镜像。
func (a *agent) imageState(ctx context.Context) (imageState, error) {
	st := imageState{names: map[string]string{}}
	var err error
	if st.images, err = a.docker.Images(ctx); err != nil {
		return st, dockerUnavailable(err)
	}
	if st.containers, err = a.docker.ImageContainers(ctx); err != nil {
		return st, dockerUnavailable(err)
	}
	st.held = append(st.held, heldRef{ref: docker.LinkImage, hold: &nagiv1.ImageHold{Kind: holdSystem}})
	for _, c := range a.projects.List(ctx) {
		st.names[c.Spec.GetProjectId()] = c.Spec.GetProjectName()
		st.held = append(st.held, specRefs(c.Spec, holdDeployed)...)
	}
	// 副本：热备提前拉好的镜像、作为备份节点恢复时要用的镜像，配置是面板登记副本时给的。
	entries, _ := os.ReadDir(a.replicas.dir)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(a.replicas.dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		m := &replicaMeta{}
		if json.Unmarshal(raw, m) != nil {
			continue
		}
		_, sp, _, err := a.replicas.load(m.ProjectID, m.Container)
		if err != nil {
			continue
		}
		kind := holdBackup
		if a.failovers.get(m.ProjectID+"-"+m.Container) != nil {
			kind = holdStandby
		}
		if _, ok := st.names[sp.GetProjectId()]; !ok {
			st.names[sp.GetProjectId()] = sp.GetProjectName()
		}
		st.held = append(st.held, specRefs(sp, kind)...)
	}
	return st, nil
}

func dockerUnavailable(err error) error {
	return status.Error(codes.Unavailable, fmt.Sprintf("无法连接 Docker：%s", err))
}

// ---- AgentService ----

func (s *agentService) ListImages(ctx context.Context, _ *nagiv1.ListImagesRequest) (*nagiv1.NodeImages, error) {
	// 统计构建缓存可能要几秒，和列镜像同时进行。
	cache := make(chan int64, 1)
	go func() {
		n, _ := s.node.docker.BuildCacheReclaimable(ctx)
		cache <- n
	}()
	st, err := s.node.imageState(ctx)
	if err != nil {
		return nil, err
	}
	out := computeImages(st)
	out.BuildCacheBytes = <-cache
	return out, nil
}

// deleteRefusal 返回不能删除这个镜像的原因，可以删时为空。
func deleteRefusal(ni *nagiv1.NodeImage) string {
	if len(ni.GetUsers()) > 0 {
		names := make([]string, 0, len(ni.GetUsers()))
		for _, u := range ni.GetUsers() {
			names = append(names, u.GetName())
		}
		return fmt.Sprintf("镜像正在被容器 %s 使用", strings.Join(names, "、"))
	}
	if len(ni.GetHolds()) > 0 {
		return "Nagi 需要这个镜像，不能删除"
	}
	return ""
}

func (s *agentService) DeleteImages(ctx context.Context, req *nagiv1.DeleteImagesRequest) (out *nagiv1.DeleteImagesResponse, retErr error) {
	if len(req.GetImages()) == 0 || len(req.GetImages()) > maxImageBatch {
		return nil, status.Errorf(codes.InvalidArgument, "一次可删除 1 至 %d 个镜像", maxImageBatch)
	}
	taskID, started := rand.Text(), time.Now()
	s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.delete",
		Data: map[string]any{"task_id": taskID, "phase": "started", "requested": len(req.GetImages())}})
	defer func() {
		data := map[string]any{"task_id": taskID, "phase": "finished", "result": "ok", "duration_ms": time.Since(started).Milliseconds()}
		if retErr != nil {
			data["result"], data["error"] = "failed", retErr.Error()
		} else {
			failed := 0
			for _, r := range out.GetResults() {
				if r.GetError() != "" {
					failed++
				}
			}
			data["completed"], data["failed"] = len(out.GetResults())-failed, failed
			if failed > 0 {
				data["result"] = "partial"
				if failed == len(out.GetResults()) {
					data["result"] = "failed"
				}
			}
		}
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.delete", Data: data})
	}()
	st, err := s.node.imageState(ctx)
	if err != nil {
		return nil, err
	}
	out = &nagiv1.DeleteImagesResponse{}
	list := computeImages(st)
	for _, ref := range req.GetImages() {
		// Persist each completed decision before attempting the next image. The
		// same initial snapshot and Docker's non-forced checks still apply.
		for _, r := range deleteImages(ctx, s.node.docker, list, []*nagiv1.ImageRef{ref}) {
			out.Results = append(out.Results, r)
			data := map[string]any{"task_id": taskID, "phase": "item", "tag": r.GetTag(), "result": "ok"}
			if r.GetError() != "" {
				data["result"], data["error"] = "failed", r.GetError()
				s.node.log.Warn("删除镜像失败", "image", r.GetId(), "tag", r.GetTag(), "err", r.GetError())
			}
			s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.delete", Object: r.GetId(), Data: data})
		}
	}
	return out, nil
}

// imageRemover 是删除镜像用到的 Docker 接口，测试时替换。
type imageRemover interface {
	RemoveImage(ctx context.Context, ref string) (bool, error)
	ImageID(ctx context.Context, ref string) (string, error)
}

// deleteImages 按刚算出的列表逐项检查后删除：有容器使用的、Nagi 要用的拒绝；只删标签时标签必须属于这个镜像。
func deleteImages(ctx context.Context, rm imageRemover, list *nagiv1.NodeImages, refs []*nagiv1.ImageRef) []*nagiv1.ImageDeleteResult {
	byID := map[string]*nagiv1.NodeImage{}
	for _, ni := range list.GetImages() {
		byID[ni.GetId()] = ni
	}
	var out []*nagiv1.ImageDeleteResult
	for _, r := range refs {
		res := &nagiv1.ImageDeleteResult{Id: r.GetId(), Tag: r.GetTag()}
		out = append(out, res)
		ni := byID[r.GetId()]
		switch {
		case ni == nil:
			res.Error = "镜像不存在或已删除"
		case deleteRefusal(ni) != "":
			res.Error = deleteRefusal(ni)
		case r.GetTag() != "" && !slices.Contains(ni.GetTags(), r.GetTag()):
			res.Error = fmt.Sprintf("镜像没有标签 %s", r.GetTag())
		case r.GetTag() != "":
			if _, err := rm.RemoveImage(ctx, r.GetTag()); err != nil {
				res.Error = fmt.Sprintf("删除失败：%s", err)
			}
		case r.GetCheckTags() && len(newTags(ni.GetTags(), r.GetTags())) > 0:
			// 删除整个镜像会去掉它所有的标签：预览之后新加的标签没有给用户看过，不跟着删。
			res.Error = fmt.Sprintf("预览之后镜像多了标签 %s，已跳过，请重新确认", strings.Join(newTags(ni.GetTags(), r.GetTags()), "、"))
		default:
			if err := removeWholeImage(ctx, rm, ni); err != nil {
				res.Error = fmt.Sprintf("删除失败：%s", err)
			}
		}
	}
	return out
}

// newTags 返回 now 里有、seen 里没有的标签。
func newTags(now, seen []string) []string {
	var out []string
	for _, t := range now {
		if !slices.Contains(seen, t) {
			out = append(out, t)
		}
	}
	return out
}

// removeWholeImage 删除整个镜像：逐个去掉标签和仓库摘要，最后按 ID 删除。都不强制，镜像被容器用上时 Docker 拒绝。
func removeWholeImage(ctx context.Context, rm imageRemover, ni *nagiv1.NodeImage) error {
	for _, t := range ni.GetTags() {
		deleted, err := rm.RemoveImage(ctx, t)
		if err != nil {
			return err
		}
		if deleted {
			return nil
		}
	}
	if _, err := rm.ImageID(ctx, ni.GetId()); err != nil {
		return nil // 去掉最后一个标签时已经删了
	}
	if deleted, err := rm.RemoveImage(ctx, ni.GetId()); err == nil && deleted {
		return nil
	}
	// 被几个仓库的摘要引用时按 ID 删不掉，逐个去掉摘要。
	for _, d := range ni.GetDigests() {
		deleted, err := rm.RemoveImage(ctx, d)
		if err != nil {
			return err
		}
		if deleted {
			return nil
		}
	}
	if _, err := rm.ImageID(ctx, ni.GetId()); err == nil {
		_, err = rm.RemoveImage(ctx, ni.GetId())
		return err
	}
	return nil
}

func (s *agentService) PruneImages(ctx context.Context, req *nagiv1.PruneImagesRequest) (out *nagiv1.PruneImagesResponse, retErr error) {
	if len(req.GetIds()) > maxImageBatch {
		return nil, status.Errorf(codes.InvalidArgument, "一次最多清理 %d 个镜像", maxImageBatch)
	}
	if len(req.GetIds()) == 0 && !req.GetBuildCache() {
		return nil, status.Error(codes.InvalidArgument, "没有可清理的内容")
	}
	taskID, started := rand.Text(), time.Now()
	s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.prune",
		Data: map[string]any{"task_id": taskID, "phase": "started", "requested": len(req.GetIds()), "build_cache": req.GetBuildCache()}})
	defer func() {
		data := map[string]any{"task_id": taskID, "phase": "finished", "result": "ok", "duration_ms": time.Since(started).Milliseconds(),
			"deleted": out.GetDeleted(), "failed": len(out.GetFailed()), "skipped": out.GetSkipped(), "build_cache_freed": out.GetBuildCacheFreed()}
		if retErr != nil {
			data["result"], data["error"] = "failed", retErr.Error()
		} else if len(out.GetFailed()) > 0 {
			data["result"] = "partial"
		}
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.prune", Data: data})
	}()
	st, err := s.node.imageState(ctx)
	if err != nil {
		return nil, err
	}
	out = &nagiv1.PruneImagesResponse{}
	list := computeImages(st)
	// Deduplicate across the batch exactly as before, while retaining each
	// rejected/duplicate request as a skipped decision rather than a deletion.
	targets := pruneTargets(req.GetIds(), list.GetPruneIds())
	allowed := make(map[string]bool, len(targets))
	for _, id := range targets {
		allowed[id] = true
	}
	for _, id := range req.GetIds() {
		data := map[string]any{"task_id": taskID, "phase": "item", "result": "skipped"}
		if !allowed[id] {
			out.Skipped++
		} else {
			delete(allowed, id)
			r := pruneImages(ctx, s.node.docker, list, []string{id})
			out.Deleted += r.GetDeleted()
			out.DeletedIds = append(out.DeletedIds, r.GetDeletedIds()...)
			out.Failed = append(out.Failed, r.GetFailed()...)
			data["result"] = "ok"
			if len(r.GetFailed()) > 0 {
				data["result"], data["error"] = "failed", r.GetFailed()[0].GetError()
			}
		}
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.prune", Object: id, Data: data})
	}
	if req.GetBuildCache() {
		freed, err := s.node.docker.PruneBuildCache(ctx)
		data := map[string]any{"task_id": taskID, "phase": "build_cache", "result": "ok", "freed_bytes": freed}
		if err != nil {
			out.Failed = append(out.Failed, &nagiv1.ImageDeleteResult{Error: fmt.Sprintf("清理构建缓存失败：%s", err)})
			data["result"], data["error"] = "failed", err.Error()
		}
		out.BuildCacheFreed = freed
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.prune", Data: data})
	}
	s.node.log.Info("已清理镜像", "deleted", out.Deleted, "failed", len(out.Failed), "skipped", out.Skipped, "build_cache_freed", out.BuildCacheFreed)
	return out, nil
}

// pruneImages 删除请求里列出、按刚算出的列表仍然可以清理的镜像。
func pruneImages(ctx context.Context, rm imageRemover, list *nagiv1.NodeImages, ids []string) *nagiv1.PruneImagesResponse {
	byID := map[string]*nagiv1.NodeImage{}
	for _, ni := range list.GetImages() {
		byID[ni.GetId()] = ni
	}
	out := &nagiv1.PruneImagesResponse{}
	targets := pruneTargets(ids, list.GetPruneIds())
	for _, id := range targets {
		if err := removeWholeImage(ctx, rm, byID[id]); err != nil {
			out.Failed = append(out.Failed, &nagiv1.ImageDeleteResult{Id: id, Error: fmt.Sprintf("删除失败：%s", err)})
			continue
		}
		out.Deleted++
		out.DeletedIds = append(out.DeletedIds, id)
	}
	out.Skipped = int32(len(ids) - len(targets))
	return out
}

// pruneTargets 是请求里列出、现在仍然可以清理的镜像（去重，按请求的顺序）。
func pruneTargets(requested, candidates []string) []string {
	ok := map[string]bool{}
	for _, id := range candidates {
		ok[id] = true
	}
	var out []string
	for _, id := range requested {
		if ok[id] {
			out = append(out, id)
			delete(ok, id)
		}
	}
	return out
}

// normalizeImageRef 检查镜像引用（Docker 的引用语法），返回补全了标签的完整引用和仓库域名。
// 不接受镜像 ID 和本地的短 ID：拉取只能指定仓库里的镜像。
func normalizeImageRef(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", "", status.Error(codes.InvalidArgument, "请填写镜像")
	case len(s) > maxImageRef:
		return "", "", status.Errorf(codes.InvalidArgument, "镜像不能超过 %d 个字符", maxImageRef)
	case strings.ContainsAny(s, " \t\r\n") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "sha256:"):
		return "", "", status.Error(codes.InvalidArgument, "镜像格式不正确，例如 nginx:1.27 或 ghcr.io/owner/app:tag")
	}
	named, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return "", "", status.Error(codes.InvalidArgument, "镜像格式不正确，例如 nginx:1.27 或 ghcr.io/owner/app:tag")
	}
	named = reference.TagNameOnly(named)
	return named.String(), reference.Domain(named), nil
}

// maxPulls 是一个节点同时进行的拉取数，pullSlots 是占用的名额。
const maxPulls = 2

var pullSlots = make(chan struct{}, maxPulls)

// pullDiskInterval 是拉取过程中检查磁盘剩余空间的间隔。
var pullDiskInterval = 5 * time.Second

// watchPullDisk 在拉取过程中定期检查磁盘（check 是 metrics.CheckDisk），剩余空间不足时以它的错误为原因取消拉取。
func watchPullDisk(ctx context.Context, check func() error, stop context.CancelCauseFunc) {
	t := time.NewTicker(pullDiskInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := check(); err != nil {
				stop(err)
				return
			}
		}
	}
}

func (s *agentService) PullImage(req *nagiv1.PullImageRequest, stream nagiv1.AgentService_PullImageServer) (retErr error) {
	ref, domain, err := normalizeImageRef(req.GetImage())
	if err != nil {
		return err
	}
	// 每个节点同时最多 maxPulls 个拉取：被攻破的面板或令牌不能一下子发起很多拉取占满带宽和磁盘。
	select {
	case pullSlots <- struct{}{}:
		defer func() { <-pullSlots }()
	default:
		return status.Errorf(codes.ResourceExhausted, "节点上已有 %d 个镜像在拉取，请稍后再试", maxPulls)
	}
	// 调用方取消时停止（PullImage 不在 detach 的范围内，见 isFollowStream）。
	ctx, cancel := context.WithTimeout(stream.Context(), pullTimeout)
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	a := s.node
	taskID, started := rand.Text(), time.Now()
	pulled := false
	a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.pull", Object: ref,
		Data: map[string]any{"task_id": taskID, "phase": "started"}})
	defer func() {
		data := map[string]any{"task_id": taskID, "phase": "finished", "result": "ok", "pulled": pulled, "duration_ms": time.Since(started).Milliseconds()}
		if retErr != nil {
			data["result"], data["error"], data["code"] = "failed", retErr.Error(), status.Code(retErr).String()
		}
		a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.pull", Object: ref, Data: data})
	}()
	if root, err := a.docker.DockerRoot(ctx); err == nil && root != "" {
		if err := metrics.CheckDisk(root, 0); err != nil {
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		// 拉取过程中也定期检查：镜像很大时，开始时够用不代表拉完还够。空间不足就停下。
		go watchPullDisk(ctx, func() error { return metrics.CheckDisk(root, 0) }, stop)
	}
	a.log.Info("拉取镜像", "image", ref)
	err = a.docker.Pull(ctx, ref, globalRegistryAuth(ctx, domain), func(p docker.PullProgress) {
		a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "image.pull", Object: ref,
			Data: map[string]any{"task_id": taskID, "phase": "progress", "status": p.Status, "current": p.Current,
				"total": p.Total, "layers_done": p.LayersDone, "layers": p.Layers}})
		_ = stream.Send(&nagiv1.ImagePullEvent{Status: p.Status, Current: p.Current, Total: p.Total, LayersDone: int32(p.LayersDone), Layers: int32(p.Layers)})
	})
	var low *metrics.LowDiskError
	switch {
	case errors.As(context.Cause(ctx), &low):
		a.log.Warn("拉取镜像时磁盘空间不足，已停止", "image", ref, "err", low)
		return status.Error(codes.ResourceExhausted, low.Error())
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "拉取超过 30 分钟，已停止")
	case errors.Is(ctx.Err(), context.Canceled):
		return status.Error(codes.Canceled, "已取消拉取")
	case err != nil:
		a.log.Warn("拉取镜像失败", "image", ref, "err", err)
		return status.Error(codes.FailedPrecondition, fmt.Sprintf("拉取镜像失败：%s", err))
	}
	pulled = true
	id, _ := a.docker.ImageID(ctx, ref)
	return stream.Send(&nagiv1.ImagePullEvent{Status: "Pull complete", Done: true, Id: id})
}

// ---- 全局的 Docker 登录信息 ----

// globalDockerConfig 返回节点进程（root）全局的 Docker 配置文件：DOCKER_CONFIG 或 ~/.docker/config.json。
// 项目登录的仓库只属于项目（容器目录里的 docker/config.json），不用于这里的拉取。
func globalDockerConfig() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

var credHelperName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// globalRegistryAuth 从全局的 Docker 配置里取出仓库的登录信息（凭据助手优先），编码成 Docker 接口要的格式；没有时为空。
func globalRegistryAuth(ctx context.Context, domain string) string {
	path := globalDockerConfig()
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	server := domain
	if domain == "docker.io" {
		server = "https://index.docker.io/v1/"
	}
	helper := cfg.CredHelpers[domain]
	if helper == "" {
		helper = cfg.CredHelpers[server]
	}
	if helper == "" {
		helper = cfg.CredsStore
	}
	if helper == "" {
		return project.RegistryAuth(raw, domain)
	}
	if !credHelperName.MatchString(helper) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(server)
	b, err := cmd.Output()
	if err != nil {
		return "" // 没有登录过这个仓库时助手也会报错，按匿名拉取
	}
	var cred struct{ Username, Secret string }
	if json.Unmarshal(b, &cred) != nil || cred.Secret == "" {
		return ""
	}
	auth := map[string]string{"serveraddress": server}
	if cred.Username == "<token>" {
		auth["identitytoken"] = cred.Secret
	} else {
		auth["username"], auth["password"] = cred.Username, cred.Secret
	}
	js, _ := json.Marshal(auth)
	return base64.URLEncoding.EncodeToString(js)
}
