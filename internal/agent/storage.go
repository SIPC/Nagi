package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 磁盘占用（GetStorage）：只读的统计，给管理员排查磁盘快满时用（见 docs/mcp.md#查看）。不清理任何东西。

const (
	// storageWalkBudget 是统计所有目录总共能花的时间，超过的目录标记为没算完。
	storageWalkBudget = 20 * time.Second
	// storageWalkFiles 是每个目录最多数的文件数。
	storageWalkFiles     = 200_000
	storageLargestImages = 10
)

func (s *agentService) GetStorage(ctx context.Context, _ *nagiv1.GetStorageRequest) (*nagiv1.NodeStorage, error) {
	out := &nagiv1.NodeStorage{}
	for _, d := range s.metrics.Disks() {
		out.Disks = append(out.Disks, &nagiv1.DiskUsage{Mount: d.Mount, TotalBytes: d.Total, UsedBytes: d.Used})
	}
	if du, err := s.node.docker.DiskUsage(ctx); err != nil {
		out.DockerError = err.Error()
	} else {
		total := func(count, active, size, reclaimable int64) *nagiv1.StorageTotal {
			return &nagiv1.StorageTotal{Count: count, Active: active, Bytes: size, ReclaimableBytes: reclaimable}
		}
		out.Images = total(du.Images.TotalCount, du.Images.ActiveCount, du.Images.TotalSize, du.Images.Reclaimable)
		out.Containers = total(du.Containers.TotalCount, du.Containers.ActiveCount, du.Containers.TotalSize, du.Containers.Reclaimable)
		out.Volumes = total(du.Volumes.TotalCount, du.Volumes.ActiveCount, du.Volumes.TotalSize, du.Volumes.Reclaimable)
		out.BuildCache = total(du.BuildCache.TotalCount, du.BuildCache.ActiveCount, du.BuildCache.TotalSize, du.BuildCache.Reclaimable)
		images := du.Images.Items
		sort.Slice(images, func(i, j int) bool { return images[i].Size > images[j].Size })
		for _, im := range images[:min(len(images), storageLargestImages)] {
			id := strings.TrimPrefix(im.ID, "sha256:")
			name := id[:min(12, len(id))]
			if len(im.RepoTags) > 0 && im.RepoTags[0] != "<none>:<none>" {
				name = im.RepoTags[0]
			} else if len(im.RepoDigests) > 0 {
				name = im.RepoDigests[0]
			}
			out.LargestImages = append(out.LargestImages, &nagiv1.StorageImage{Name: name, Bytes: im.Size, Containers: im.Containers})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, storageWalkBudget)
	defer cancel()
	for _, c := range s.projects.List(ctx) {
		sp := c.Spec
		u := &nagiv1.ContainerDirUsage{ProjectId: sp.GetProjectId(), ProjectName: sp.GetProjectName(), Container: sp.GetName(), Dir: sp.GetDir()}
		var p1, p2, p3 bool
		u.DataBytes, p1 = dirBytes(ctx, filepath.Join(sp.GetDir(), "data"))
		u.VolumesBytes, p2 = dirBytes(ctx, filepath.Join(sp.GetDir(), "volumes"))
		u.BackupsBytes, p3 = dirBytes(ctx, filepath.Join(sp.GetDir(), "backups"))
		u.Partial = p1 || p2 || p3
		out.ContainerDirs = append(out.ContainerDirs, u)
	}
	sort.Slice(out.ContainerDirs, func(i, j int) bool {
		a, b := out.ContainerDirs[i], out.ContainerDirs[j]
		return a.DataBytes+a.VolumesBytes+a.BackupsBytes > b.DataBytes+b.VolumesBytes+b.BackupsBytes
	})
	out.ReplicasBytes, _ = dirBytes(ctx, s.node.replicas.dir)
	return out, nil
}

// dirBytes 统计目录下普通文件的总大小，不跟随符号链接。时间或文件数超出时停下，partial 为 true。目录不存在时是 0。
func dirBytes(ctx context.Context, dir string) (total int64, partial bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, false
	}
	defer root.Close()
	stop := errors.New("stop")
	files := 0
	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的文件不计。
		}
		if files >= storageWalkFiles || ctx.Err() != nil {
			return stop
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				files++
				total += fi.Size()
			}
		}
		return nil
	})
	return total, errors.Is(err, stop)
}
