package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 节点镜像（见 docs/agent.md#节点镜像、docs/mcp.md#节点镜像）：只给管理员。删除、清理要勾「可修改」，
// 而且和界面的高权限确认对应，都分两步：先预览，用户同意后带确认码执行。能不能删由节点判断。

// maxListedImages 是 list_node_images 最多列出的镜像数。
const maxListedImages = 300

func mb(b int64) float64 { return math.Round(float64(b)/(1<<20)*10) / 10 }

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(12, len(id))]
}

// imageLabel 是镜像在预览里的名字：第一个标签，没有标签时是短 ID。
func imageLabel(im *nagiv1.NodeImage) string {
	if len(im.GetTags()) > 0 {
		return im.GetTags()[0]
	}
	return shortImageID(im.GetId())
}

// imageUsers 是使用这个镜像的容器。
func imageUsers(im *nagiv1.NodeImage) []string {
	out := []string{}
	for _, u := range im.GetUsers() {
		name := u.GetName()
		switch {
		case u.GetSystem():
			name = "project network proxy"
		case u.GetContainer() != "":
			name = cmpOr(u.GetProjectName(), u.GetProjectId()) + "/" + u.GetContainer()
		}
		if !u.GetRunning() {
			name += " (stopped)"
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// imageHolds 是 Nagi 需要这个镜像的原因。
func imageHolds(im *nagiv1.NodeImage) []string {
	kinds := map[string]string{"deployed": "deployed version", "standby": "hot standby", "backup": "backup restore", "system": "project network proxy"}
	out := []string{}
	for _, h := range im.GetHolds() {
		s := cmpOr(kinds[h.GetKind()], h.GetKind())
		if h.GetContainer() != "" {
			s += " of " + cmpOr(h.GetProjectName(), h.GetProjectId()) + "/" + h.GetContainer()
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// imageRefusal 是节点会拒绝删除的原因，可以删时为空。
func imageRefusal(im *nagiv1.NodeImage) string {
	if u := imageUsers(im); len(u) > 0 {
		return "used by container " + strings.Join(u, ", ")
	}
	if h := imageHolds(im); len(h) > 0 {
		return "needed by Nagi: " + strings.Join(h, ", ")
	}
	return ""
}

// nodeImages 按名称或 ID 找到节点并列出它的镜像。
func (c *call) nodeImages(ref string) (*nagiv1.Node, *nagiv1.NodeImages, error) {
	nodes, err := c.nodes()
	if err != nil {
		return nil, nil, err
	}
	n, err := findNode(nodes, ref)
	if err != nil {
		return nil, nil, err
	}
	res, err := c.h.clients.nodes.ListNodeImages(c.ctx, connect.NewRequest(&nagiv1.NodeRef{NodeId: n.GetId()}))
	if err != nil {
		return nil, nil, err
	}
	return n, res.Msg, nil
}

// confirmCode 是删除、清理的确认码：由节点和要做的事算出，预览之后有变化就对不上。
func confirmCode(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// ---- 查看 ----

type ListNodeImagesArgs struct {
	Node   string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Filter string `json:"filter,omitempty" jsonschema:"only images whose tag, ID or user contains this text"`
}

type imageView struct {
	ID        string   `json:"id"`
	Tags      []string `json:"tags"`
	SizeMB    float64  `json:"size_mb"`
	UniqueMB  float64  `json:"unique_mb"`
	Created   string   `json:"created"`
	UsedBy    []string `json:"used_by,omitempty"`
	NeededBy  []string `json:"needed_by_nagi,omitempty"`
	Deletable bool     `json:"deletable"`
}

func toolListNodeImages(c *call, in ListNodeImagesArgs) (any, error) {
	n, list, err := c.nodeImages(in.Node)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(in.Filter))
	out := struct {
		Node         string      `json:"node"`
		Count        int         `json:"count"`
		Images       []imageView `json:"images"`
		Truncated    bool        `json:"truncated,omitempty"`
		PruneImages  int         `json:"prune_candidates"`
		PruneFreedMB float64     `json:"prune_at_least_freed_mb"`
		BuildCacheMB float64     `json:"build_cache_reclaimable_mb"`
		Note         string      `json:"note"`
	}{Node: n.GetName(), Images: []imageView{}, PruneImages: len(list.GetPruneIds()), PruneFreedMB: mb(list.GetPruneBytes()), BuildCacheMB: mb(list.GetBuildCacheBytes()),
		Note: "images used by a container or needed by Nagi cannot be deleted or cleaned up. Delete with delete_node_images, clean up with prune_node_images."}
	for _, im := range list.GetImages() {
		users, holds := imageUsers(im), imageHolds(im)
		if q != "" && !strings.Contains(strings.ToLower(strings.Join(append(append(append([]string{im.GetId()}, im.GetTags()...), users...), holds...), "\n")), q) {
			continue
		}
		out.Count++
		if len(out.Images) >= maxListedImages {
			out.Truncated = true
			continue
		}
		out.Images = append(out.Images, imageView{ID: shortImageID(im.GetId()), Tags: append([]string{}, im.GetTags()...), SizeMB: mb(im.GetSize()), UniqueMB: mb(im.GetUniqueSize()),
			Created: ts(im.GetCreated()), UsedBy: users, NeededBy: holds, Deletable: len(users) == 0 && len(holds) == 0})
	}
	return out, nil
}

// ---- 删除 ----

type DeleteNodeImagesArgs struct {
	Node    string   `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Images  []string `json:"images,omitempty" jsonschema:"images to delete with all their tags: a tag (nginx:1.27) or an image ID (at least 12 characters, from list_node_images)"`
	Tags    []string `json:"tags,omitempty" jsonschema:"tags to remove; the image is kept while it has other tags, and deleted with its last tag"`
	Confirm string   `json:"confirm,omitempty" jsonschema:"omit to get a preview; after the user agreed, call again with the same arguments and confirm set to the code from the preview"`
}

// findImage 按标签或 ID（至少 12 位）找镜像。
func findImage(list *nagiv1.NodeImages, ref string) (*nagiv1.NodeImage, error) {
	ref = strings.TrimSpace(ref)
	id := strings.TrimPrefix(ref, "sha256:")
	var found []*nagiv1.NodeImage
	for _, im := range list.GetImages() {
		if slices.Contains(im.GetTags(), ref) || !strings.Contains(ref, ":") && slices.Contains(im.GetTags(), ref+":latest") ||
			len(id) >= 12 && strings.HasPrefix(strings.TrimPrefix(im.GetId(), "sha256:"), id) {
			found = append(found, im)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("image %q not found on the node (see list_node_images)", ref)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%q matches %d images; use a longer ID", ref, len(found))
}

func toolDeleteNodeImages(c *call, in DeleteNodeImagesArgs) (any, error) {
	if len(in.Images)+len(in.Tags) == 0 {
		return nil, errors.New("give images or tags to delete")
	}
	n, list, err := c.nodeImages(in.Node)
	if err != nil {
		return nil, err
	}
	type planned struct {
		ref   *nagiv1.ImageRef
		label string
	}
	var plan []planned
	refused := map[string]string{}
	seen := map[string]bool{}
	add := func(im *nagiv1.NodeImage, tag, label string) {
		if r := imageRefusal(im); r != "" {
			refused[label] = r
			return
		}
		if k := im.GetId() + "|" + tag; !seen[k] {
			seen[k] = true
			plan = append(plan, planned{&nagiv1.ImageRef{Id: im.GetId(), Tag: tag}, label})
		}
	}
	for _, ref := range in.Images {
		im, err := findImage(list, ref)
		if err != nil {
			return nil, err
		}
		label := imageLabel(im)
		if len(im.GetTags()) > 1 {
			label += " (all tags: " + strings.Join(im.GetTags(), ", ") + ")"
		}
		add(im, "", label)
	}
	for _, tag := range in.Tags {
		im, err := findImage(list, tag)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(im.GetTags(), tag) {
			return nil, fmt.Errorf("%q is not a tag; use images to delete by ID", tag)
		}
		label := "tag " + tag
		if len(im.GetTags()) == 1 {
			label += " (its last tag: the image is deleted)"
		}
		add(im, tag, label)
	}
	keys := make([]string, 0, len(plan))
	labels := make([]string, 0, len(plan))
	for _, p := range plan {
		keys = append(keys, p.ref.GetId()+"|"+p.ref.GetTag())
		labels = append(labels, p.label)
	}
	sort.Strings(keys)
	code := confirmCode(append([]string{"delete", n.GetId()}, keys...)...)
	if in.Confirm != code {
		if len(plan) == 0 {
			return map[string]any{"node": n.GetName(), "refused": refused, "note": "nothing can be deleted: the node refuses images used by a container or needed by Nagi"}, nil
		}
		out := preview("delete_node_images", code, fmt.Sprintf("these will be deleted from node %s now; deleted images can be pulled again.", n.GetName()),
			map[string]any{"node": n.GetName(), "delete": labels})
		if len(refused) > 0 {
			out["refused"] = refused
		}
		if in.Confirm != "" {
			out["note"] = "the confirm code does not match these images on the node now; show this preview to the user again"
		}
		return out, nil
	}
	refs := make([]*nagiv1.ImageRef, 0, len(plan))
	for _, p := range plan {
		refs = append(refs, p.ref)
	}
	res, err := c.h.clients.nodes.DeleteNodeImages(c.ctx, connect.NewRequest(&nagiv1.DeleteNodeImagesRequest{NodeId: n.GetId(), Images: refs}))
	if err != nil {
		return nil, err
	}
	type result struct {
		Item  string `json:"item"`
		Error string `json:"error,omitempty"`
	}
	out := struct {
		Node    string   `json:"node"`
		Deleted int      `json:"deleted"`
		Results []result `json:"results"`
		Success bool     `json:"success"`
		Error   string   `json:"error,omitempty"`
	}{Node: n.GetName(), Results: []result{}}
	for i, r := range res.Msg.GetResults() {
		item := cmpOr(r.GetTag(), shortImageID(r.GetId()))
		if i < len(plan) {
			item = plan[i].label
		}
		out.Results = append(out.Results, result{item, r.GetError()})
		if r.GetError() == "" {
			out.Deleted++
		}
	}
	out.Success = out.Deleted > 0
	if !out.Success {
		out.Error = "nothing was deleted"
	}
	return out, nil
}

// ---- 一键清理 ----

type PruneNodeImagesArgs struct {
	Node       string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	BuildCache bool   `json:"build_cache,omitempty" jsonschema:"also clean up unused build cache. Default false"`
	Confirm    string `json:"confirm,omitempty" jsonschema:"omit to get a preview; after the user agreed, call again with the same arguments and confirm set to the code from the preview"`
}

func toolPruneNodeImages(c *call, in PruneNodeImagesArgs) (any, error) {
	n, list, err := c.nodeImages(in.Node)
	if err != nil {
		return nil, err
	}
	ids := append([]string{}, list.GetPruneIds()...)
	sort.Strings(ids)
	cache := in.BuildCache && list.GetBuildCacheBytes() > 0
	if len(ids) == 0 && !cache {
		return map[string]any{"node": n.GetName(), "note": "nothing to clean up: every image is used by a container or needed by Nagi", "build_cache_reclaimable_mb": mb(list.GetBuildCacheBytes())}, nil
	}
	code := confirmCode(append([]string{"prune", n.GetId(), fmt.Sprint(cache)}, ids...)...)
	if in.Confirm != code {
		type item struct {
			Image  string  `json:"image"`
			SizeMB float64 `json:"size_mb"`
		}
		items := []item{}
		for _, im := range list.GetImages() {
			if slices.Contains(ids, im.GetId()) {
				items = append(items, item{cmpOr(strings.Join(im.GetTags(), ", "), shortImageID(im.GetId())), mb(im.GetSize())})
			}
		}
		text := fmt.Sprintf("%d images that no container uses and Nagi does not need will be deleted from node %s, freeing at least %.1f MB (layers shared among them are not counted). They can be pulled again.", len(ids), n.GetName(), mb(list.GetPruneBytes()))
		if cache {
			text += fmt.Sprintf(" Unused build cache (%.1f MB) is cleaned up too.", mb(list.GetBuildCacheBytes()))
		}
		out := preview("prune_node_images", code, text, map[string]any{"node": n.GetName(), "images": items, "build_cache": cache})
		if in.Confirm != "" {
			out["note"] = "the images on the node changed since the preview; show this preview to the user again"
		}
		return out, nil
	}
	res, err := c.h.clients.nodes.PruneNodeImages(c.ctx, connect.NewRequest(&nagiv1.PruneNodeImagesRequest{NodeId: n.GetId(), Ids: ids, BuildCache: cache}))
	if err != nil {
		return nil, err
	}
	failed := []string{}
	for _, f := range res.Msg.GetFailed() {
		failed = append(failed, cmpOr(shortImageID(f.GetId()), "build cache")+": "+f.GetError())
	}
	note := ""
	if res.Msg.GetSkipped() > 0 {
		note = "skipped images were used or removed after the preview"
	}
	return struct {
		Node            string   `json:"node"`
		Deleted         int32    `json:"deleted"`
		Skipped         int32    `json:"skipped,omitempty"`
		Failed          []string `json:"failed,omitempty"`
		BuildCacheFreed float64  `json:"build_cache_freed_mb,omitempty"`
		Note            string   `json:"note,omitempty"`
	}{n.GetName(), res.Msg.GetDeleted(), res.Msg.GetSkipped(), failed, mb(res.Msg.GetBuildCacheFreed()), note}, nil
}

// ---- 拉取 ----

type PullNodeImageArgs struct {
	Node  string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Image string `json:"image" jsonschema:"image reference, for example nginx:1.27 or ghcr.io/owner/app:tag"`
}

func toolPullNodeImage(c *call, in PullNodeImageArgs) (any, error) {
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	stream, err := c.h.clients.nodes.PullNodeImage(c.ctx, connect.NewRequest(&nagiv1.PullNodeImageRequest{NodeId: n.GetId(), Image: in.Image}))
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var last *nagiv1.ImagePullEvent
	for stream.Receive() {
		last = stream.Msg()
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if !last.GetDone() {
		return nil, errors.New("the node stopped without finishing the pull")
	}
	return map[string]string{"node": n.GetName(), "image": in.Image, "id": shortImageID(last.GetId()),
		"note": "pulled with the node's global Docker credentials and registry mirrors"}, nil
}
