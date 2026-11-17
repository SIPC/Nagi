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
	dirSizeWorkers    = 8
)

// DirSize 是一个目录统计的结果。
type DirSize struct {
	Name     string
	Size     int64
	Files    int64
	Complete bool
	// Dir：确实是目录。符号链接和文件不统计（不跟随链接），调用方不返回它们。
	Dir bool
}

// DirSizes 并行统计 fsys 里 dir 下各个子目录 names 的大小（普通文件的字节数之和，不跟随符号链接）。
// 共用一份限额：timeout 到了、或者合计看过 maxEntries 项就停下。skip 对路径（fsys 里的，/ 分隔）返回 true 时跳过。
func DirSizes(ctx context.Context, fsys fs.FS, dir string, names []string, timeout time.Duration, maxEntries int64, skip func(p string) bool) []DirSize {
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
				out[i] = dirSize(ctx, fsys, path.Join(dir, names[i]), names[i], &seen, maxEntries, skip)
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

func dirSize(ctx context.Context, fsys fs.FS, root, name string, seen *atomic.Int64, maxEntries int64, skip func(string) bool) DirSize {
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
			}
		}
		return nil
	})
	res.Complete = !stopped
	return res
}
