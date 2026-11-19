package metrics

import (
	"fmt"
	"io"
	"sync"
)

// 磁盘剩余空间低于这两个值之一时不再写入大块数据（定时快照、副本同步、上传），免得把磁盘写满。
const (
	MinFreeBytes   = 2 << 30
	MinFreePercent = 5
)

// MinFreeInodes 和 MinFreePercent：文件系统剩余的 inode 低于其中之一时也不再写入。空文件和目录几乎不占空间，
// 只看剩余空间的话，大量的空文件能在空间还很充足时用完 inode，之后什么文件都建不了。
const MinFreeInodes = 10_000

// DiskCheckEvery 是大块写入时检查一次磁盘剩余空间的间隔（字节）。
const DiskCheckEvery = 64 << 20

// diskSpace、diskInodes 读磁盘的总量和可用量、inode 的总数和剩余数，测试时替换。
var (
	diskSpace  = DiskSpace
	diskInodes = DiskInodes
)

// LowDiskError 是磁盘剩余空间或 inode 不够、不再写入的错误。Inodes 不为 0 时是 inode 不够。
type LowDiskError struct{ Total, Free, Inodes, FreeInodes int64 }

func (e *LowDiskError) Error() string {
	if e.Inodes > 0 {
		return fmt.Sprintf("节点磁盘可用的 inode 不足（剩余 %d 个，共 %d 个）", e.FreeInodes, e.Inodes)
	}
	return fmt.Sprintf("节点磁盘剩余空间不足（剩余 %.1f GiB，共 %.1f GiB）", float64(e.Free)/(1<<30), float64(e.Total)/(1<<30))
}

// CheckDisk 在 path 所在磁盘再写入 need 字节后剩余空间会低于 MinFreeBytes 或 MinFreePercent，
// 或者剩余的 inode 已经低于 MinFreeInodes 或 MinFreePercent 时返回 *LowDiskError。读不到时不拦。
func CheckDisk(path string, need int64) error {
	total, free := diskSpace(path)
	left := free - max(need, 0)
	if total > 0 && (left < MinFreeBytes || left*100 < total*MinFreePercent) {
		return &LowDiskError{Total: total, Free: free}
	}
	// 不限制 inode 个数的文件系统（btrfs 等）总数报 0，不拦。
	if inodes, freeInodes := diskInodes(path); inodes > 0 && (freeInodes < MinFreeInodes || freeInodes*100 < inodes*MinFreePercent) {
		return &LowDiskError{Total: total, Free: free, Inodes: inodes, FreeInodes: freeInodes}
	}
	return nil
}

// DiskGuard 在大块写入时检查磁盘剩余空间：第一次写之前检查一次 Dir 所在的磁盘，之后每写 DiskCheckEvery 字节检查一次。
// 可以并发使用：同时往同一块盘写的几路（例如同一个副本的几条同步流）要共用一个，写入量合在一起算；
// 各用各的话，每一路都能在两次检查之间写将近 DiskCheckEvery，路数多了就绕过了检查。
type DiskGuard struct {
	Dir     string
	mu      sync.Mutex
	written int64 // 上次检查以后写了多少
	checked bool
}

// Check 记下即将写入 n 字节，到了检查的时候检查磁盘。
func (g *DiskGuard) Check(n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked || g.written >= DiskCheckEvery {
		if err := CheckDisk(g.Dir, 0); err != nil {
			return err
		}
		g.checked, g.written = true, 0
	}
	g.written += int64(n)
	return nil
}

// Writer 返回一个先经过 Check 再写入 w 的 io.Writer。
func (g *DiskGuard) Writer(w io.Writer) io.Writer { return guardWriter{g, w} }

type guardWriter struct {
	g *DiskGuard
	w io.Writer
}

func (w guardWriter) Write(p []byte) (int, error) {
	if err := w.g.Check(len(p)); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
