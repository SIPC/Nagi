package docker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// 节点镜像管理用到的 Docker 接口（见 docs/agent.md#节点镜像）。哪些能删由 agent 包判断，这里只是读写。

// Image 是节点上的一个镜像。
type Image struct {
	ID      string // sha256:...
	Tags    []string
	Digests []string
	Size    int64
	// SharedSize 是和别的镜像共用的层的大小，Docker 算不出来时为 -1。
	SharedSize int64
	Created    time.Time
}

// Images 列出节点上的镜像（不含构建的中间层）。
func (d *Client) Images(ctx context.Context) ([]Image, error) {
	res, err := d.c.ImageList(ctx, client.ImageListOptions{SharedSize: true})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
		im := Image{ID: s.ID, Digests: s.RepoDigests, Size: s.Size, SharedSize: s.SharedSize, Created: time.Unix(s.Created, 0)}
		for _, t := range s.RepoTags {
			if t != "<none>:<none>" {
				im.Tags = append(im.Tags, t)
			}
		}
		if len(im.Digests) == 1 && im.Digests[0] == "<none>@<none>" {
			im.Digests = nil
		}
		out = append(out, im)
	}
	return out, nil
}

// ImageContainer 是节点上的一个容器（包括已停止的）和它用的镜像。
type ImageContainer struct {
	ID      string
	Name    string
	ImageID string
	Running bool
	Labels  map[string]string
}

// ImageContainers 列出节点上所有容器用到的镜像。
func (d *Client) ImageContainers(ctx context.Context) ([]ImageContainer, error) {
	res, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]ImageContainer, 0, len(res.Items))
	for _, c := range res.Items {
		name := c.ID[:min(12, len(c.ID))]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ImageContainer{ID: c.ID, Name: name, ImageID: c.ImageID, Running: c.State == container.StateRunning, Labels: c.Labels})
	}
	return out, nil
}

// RemoveImage 删除一个镜像引用（标签、仓库@摘要或镜像 ID），不强制：有容器使用、被多个仓库引用时 Docker 拒绝。
// 去掉最后一个引用时镜像本身被删除，返回 true。
func (d *Client) RemoveImage(ctx context.Context, ref string) (deleted bool, err error) {
	res, err := d.c.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	if err != nil {
		return false, err
	}
	for _, r := range res.Items {
		if r.Deleted != "" {
			deleted = true
		}
	}
	return deleted, nil
}

// BuildCacheReclaimable 返回构建缓存里没有在用、可以清理的大小。
func (d *Client) BuildCacheReclaimable(ctx context.Context) (int64, error) {
	du, err := d.c.DiskUsage(ctx, client.DiskUsageOptions{BuildCache: true})
	if err != nil {
		return 0, err
	}
	return du.BuildCache.Reclaimable, nil
}

// PruneBuildCache 清理所有没有在用的构建缓存，返回释放的大小。
func (d *Client) PruneBuildCache(ctx context.Context) (int64, error) {
	res, err := d.c.BuildCachePrune(ctx, client.BuildCachePruneOptions{All: true})
	if err != nil {
		return 0, err
	}
	return int64(res.Report.SpaceReclaimed), nil
}

// DockerRoot 返回 Docker 的数据目录（镜像存放的位置）。
func (d *Client) DockerRoot(ctx context.Context) (string, error) {
	info, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	return info.Info.DockerRootDir, nil
}

// PullProgress 是拉取进度。
type PullProgress struct {
	Status         string
	Current, Total int64
	LayersDone     int
	Layers         int
}

// pullTracker 按层汇总 Docker 的拉取进度消息。
type pullTracker struct {
	order   []string
	layers  map[string]*layerState
	status  string
	changed bool
}

type layerState struct {
	current, total int64
	done           bool
}

func newPullTracker() *pullTracker { return &pullTracker{layers: map[string]*layerState{}} }

// add 处理一条消息。没有 id 的是整体的状态（"Pulling from ..."、"Digest: ..."）。
func (p *pullTracker) add(id, status string, current, total int64) {
	p.changed = true
	if id == "" || !isLayerStatus(status) {
		switch {
		case status == "":
		case id == "":
			p.status = status
		default:
			p.status = id + ": " + status
		}
		return
	}
	l := p.layers[id]
	if l == nil {
		l = &layerState{}
		p.layers[id] = l
		p.order = append(p.order, id)
	}
	switch status {
	case "Downloading":
		l.current = current
		if total > 0 {
			l.total = total
		}
	case "Download complete", "Verifying Checksum", "Extracting":
		if l.total > 0 {
			l.current = l.total
		}
	case "Pull complete", "Already exists":
		l.done = true
		if l.total > 0 {
			l.current = l.total
		}
	}
	p.status = status
}

func isLayerStatus(s string) bool {
	switch s {
	case "Pulling fs layer", "Waiting", "Downloading", "Verifying Checksum", "Download complete", "Extracting", "Pull complete", "Already exists", "Retrying":
		return true
	}
	return strings.HasPrefix(s, "Retrying")
}

func (p *pullTracker) snapshot() PullProgress {
	out := PullProgress{Status: p.status, Layers: len(p.order)}
	for _, id := range p.order {
		l := p.layers[id]
		out.Current += l.current
		out.Total += l.total
		if l.done {
			out.LayersDone++
		}
	}
	return out
}

// pullEvery 是发送拉取进度的最短间隔。
const pullEvery = 250 * time.Millisecond

// Pull 拉取镜像。encodedAuth 是仓库的登录信息（base64url 编码的 JSON），没有时为空。
// 镜像加速由 Docker 自己按 daemon.json 处理。ctx 取消时 Docker 停止拉取。
func (d *Client) Pull(ctx context.Context, ref, encodedAuth string, onProgress func(PullProgress)) error {
	resp, err := d.c.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: encodedAuth})
	if err != nil {
		return err
	}
	defer resp.Close()
	t := newPullTracker()
	last := time.Time{}
	for m, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if m.Error != nil {
			return errors.New(m.Error.Message)
		}
		var cur, total int64
		if m.Progress != nil {
			cur, total = m.Progress.Current, m.Progress.Total
		}
		t.add(m.ID, m.Status, cur, total)
		if time.Since(last) >= pullEvery {
			last = time.Now()
			t.changed = false
			onProgress(t.snapshot())
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.changed {
		onProgress(t.snapshot())
	}
	return nil
}

// ImageID 返回镜像引用当前对应的镜像 ID。
func (d *Client) ImageID(ctx context.Context, ref string) (string, error) {
	info, err := d.c.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}
