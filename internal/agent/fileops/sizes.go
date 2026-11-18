package fileops

import (
	"context"
	"io/fs"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

// 列目录时统计子目录大小的限额：整次统计的时长和文件数。超出时停下，已经统计到的部分标记为不完整，
// 列表不会因为一个巨大的目录变慢。
const (
	DirSizeTimeout    = 3 * time.Second
	DirSizeMaxEntries = 300_000
	// 完整统计（请求带 full）：界面对第一遍没统计完的目录再请求一次，要的是真实的大小。
	// 限额放宽到 2 分钟、5000 万项，超出仍标为不完整（这种目录界面上不显示大小）。
	// 同一个节点同时最多 fullSlots 个完整统计，其余的排队，免得磁盘被打满。
	DirSizeFullTimeout    = 2 * time.Minute
	DirSizeFullMaxEntries = 50_000_000
	dirSizeWorkers        = 8
)

// DirSize 是一个目录统计的结果。
type DirSize struct {
	Name     string
	Size     int64
	Files    int64
	Complete bool
	// TooLarge：统计到的大小超过了 maxBytes，停下了（Complete 为 false）。
	TooLarge bool
	// Dir：确实是目录。符号链接和文件不统计（不跟随链接），调用方不返回它们。
	Dir bool
}

var fullSlots = make(chan struct{}, 2)

// fullWait 是等完整统计名额的上限（测试里改短）。名额被别的统计占着、等不到时，按快速统计的限额算一遍：
// 这些目录多半还是统计不完，标为不完整，界面显示「—」；请求不会一直排着队，占不满的名额也就不会越排越长。
var fullWait = 30 * time.Second

// DirSizeLimits 返回一次统计的时限和项数上限。full 时先占一个完整统计的名额（最多等 fullWait，ctx 结束时不再等），
// 等不到时返回快速统计的限额。返回的 release 要在统计完以后调用。
func DirSizeLimits(ctx context.Context, full bool) (timeout time.Duration, maxEntries int64, release func(), err error) {
	if !full {
		return DirSizeTimeout, DirSizeMaxEntries, func() {}, nil
	}
	t := time.NewTimer(fullWait)
	defer t.Stop()
	select {
	case fullSlots <- struct{}{}:
		return DirSizeFullTimeout, DirSizeFullMaxEntries, func() { <-fullSlots }, nil
	case <-t.C:
		return DirSizeTimeout, DirSizeMaxEntries, func() {}, nil
	case <-ctx.Done():
		return 0, 0, nil, ctx.Err()
	}
}

// DirSizes 并行统计 fsys 里 dir 下各个子目录 names 的大小（普通文件的字节数之和，不跟随符号链接）。
// 共用一份限额：timeout 到了、或者合计看过 maxEntries 项就停下。maxBytes 大于 0 时，单个目录统计到的大小超过它就停下，
// 标为 TooLarge。skip 对路径（fsys 里的，/ 分隔）返回 true 时跳过。
func DirSizes(ctx context.Context, fsys fs.FS, dir string, names []string, timeout time.Duration, maxEntries, maxBytes int64, skip func(p string) bool) []DirSize {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := make([]DirSize, len(names))
	var seen atomic.Int64
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(dirSizeWorkers, len(names)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i] = dirSize(ctx, fsys, path.Join(dir, names[i]), names[i], &seen, maxEntries, maxBytes, skip)
			}
		}()
	}
	for i := range names {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

func dirSize(ctx context.Context, fsys fs.FS, root, name string, seen *atomic.Int64, maxEntries, maxBytes int64, skip func(string) bool) DirSize {
	res := DirSize{Name: name}
	if info, err := fs.Lstat(fsys, root); err != nil || !info.IsDir() {
		return res
	}
	res.Dir = true
	stopped := false
	_ = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的跳过
		}
		if seen.Add(1) > maxEntries || ctx.Err() != nil {
			stopped = true
			return fs.SkipAll
		}
		if skip != nil && skip(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				res.Size += info.Size()
				res.Files++
				if maxBytes > 0 && res.Size > maxBytes {
					res.TooLarge = true
					stopped = true
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	res.Complete = !stopped
	return res
}
