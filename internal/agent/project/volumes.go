package project

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 容器的存储（见 docs/agent.md#容器目录）：data/ 是应用文件，volumes/<名字> 是命名 volume 的数据。
// 容器能写这些目录，里面随时可能有指向外面的符号链接，所以读写都经 os.Root，见 safefs.go。

const (
	// DataVolume 代表容器的 data/ 目录。compose 的卷名不能以点开头，所以不会和命名 volume 冲突（/data 生成的卷名就是 data）。
	DataVolume = ".data"

	volumeWalkLimit   = 500_000
	volumeWalkTimeout = 10 * time.Second
)

// ListVolumes 列出容器的存储：data/、compose 里的命名 volume，以及配置里已经没有、目录还在的 volume。
func (m *Manager) ListVolumes(ctx context.Context, projectID, container string) (*nagiv1.ListVolumesResponse, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return nil, err
	}
	s := c.Spec
	mp := containerPaths{root: s.GetDir()}
	cp, err := spec.Load(ctx, s.GetConfig(), s.GetName(), s.GetSharedEnv(), mp.dataDir(), spec.ComposeProjectName(projectID, container))
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListVolumesResponse{Volumes: volumesFromCompose(cp, mp)}

	known := map[string]bool{}
	for _, v := range out.Volumes {
		known[v.GetName()] = true
	}
	if entries, err := os.ReadDir(mp.volumesDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() && !known[e.Name()] {
				out.Volumes = append(out.Volumes, &nagiv1.VolumeInfo{Name: e.Name(), Kind: "orphan", HostPath: filepath.Join(mp.volumesDir(), e.Name())})
			}
		}
	}
	sort.SliceStable(out.Volumes, func(i, j int) bool { return volumeRank(out.Volumes[i]) < volumeRank(out.Volumes[j]) })

	for _, v := range out.Volumes {
		if v.GetKind() != "unmanaged" {
			measureVolume(ctx, v)
		}
	}
	out.DiskTotalBytes, out.DiskFreeBytes = metrics.DiskSpace(s.GetDir())
	return out, nil
}

// volumesFromCompose 列出 compose 里用到的存储和挂载它们的容器。
func volumesFromCompose(cp *types.Project, mp containerPaths) []*nagiv1.VolumeInfo {
	data := &nagiv1.VolumeInfo{Name: DataVolume, Kind: "data", HostPath: mp.dataDir()}
	named := map[string]*nagiv1.VolumeInfo{}
	for key, v := range cp.Volumes {
		info := &nagiv1.VolumeInfo{Name: key, Kind: "managed", HostPath: filepath.Join(mp.volumesDir(), key)}
		if !spec.ManagedVolume(v) {
			info.Kind, info.HostPath = "unmanaged", ""
		}
		named[key] = info
	}
	for _, name := range cp.ServiceNames() {
		for _, v := range cp.Services[name].Volumes {
			mount := &nagiv1.VolumeMount{Service: name, Target: v.Target}
			switch v.Type {
			case types.VolumeTypeVolume:
				if info := named[v.Source]; info != nil {
					info.Mounts = append(info.Mounts, mount)
				}
			case types.VolumeTypeBind:
				if within(v.Source, mp.dataDir()) {
					data.Mounts = append(data.Mounts, mount)
				}
			}
		}
	}
	out := []*nagiv1.VolumeInfo{data}
	for _, v := range named {
		out = append(out, v)
	}
	return out
}

// volumeRank 决定显示顺序：有数据的 volume 在前，data/ 其次，最后是不在快照范围和没人用的。
func volumeRank(v *nagiv1.VolumeInfo) string {
	rank := map[string]string{"managed": "0", "data": "1", "unmanaged": "2", "orphan": "3"}[v.GetKind()]
	return rank + v.GetName()
}

// measureVolume 统计目录的大小、文件数和最后修改时间。文件太多或太慢时停下来，标记为不完整。
func measureVolume(ctx context.Context, v *nagiv1.VolumeInfo) {
	info, err := os.Lstat(v.GetHostPath())
	if err != nil || !info.IsDir() {
		return
	}
	root, err := os.OpenRoot(v.GetHostPath())
	if err != nil {
		return
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(ctx, volumeWalkTimeout)
	defer cancel()
	var newest time.Time
	stop := errors.New("stop")
	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的文件不计，不影响别的。
		}
		if v.Files >= volumeWalkLimit || ctx.Err() != nil {
			return stop
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			v.Files++
			v.SizeBytes += fi.Size()
		}
		return nil
	})
	v.Partial = errors.Is(err, stop)
	if !newest.IsZero() {
		v.Modified = timestamppb.New(newest)
	}
}
