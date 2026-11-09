package metrics

import (
	"fmt"
	"io"
)

// 磁盘剩余空间低于这两个值之一时不再写入大块数据（定时快照、副本同步、上传），免得把磁盘写满。
const (
	MinFreeBytes   = 2 << 30
	MinFreePercent = 5
)

// DiskCheckEvery 是大块写入时检查一次磁盘剩余空间的间隔（字节）。
const DiskCheckEvery = 64 << 20

// diskSpace 读磁盘的总量和可用量，测试时替换。
var diskSpace = DiskSpace

// LowDiskError 是磁盘剩余空间不够、不再写入的错误。
type LowDiskError struct{ Total, Free int64 }

func (e *LowDiskError) Error() string {
	return fmt.Sprintf("节点磁盘剩余空间不足（剩余 %.1f GiB，共 %.1f GiB）", float64(e.Free)/(1<<30), float64(e.Total)/(1<<30))
}

// CheckDisk 在 path 所在磁盘再写入 need 字节后剩余空间会低于 MinFreeBytes 或 MinFreePercent 时返回 *LowDiskError。
// 读不到时不拦。
func CheckDisk(path string, need int64) error {
	total, free := diskSpace(path)
	left := free - max(need, 0)
	if total <= 0 || left >= MinFreeBytes && left*100 >= total*MinFreePercent {
		return nil
	}
	return &LowDiskError{Total: total, Free: free}
}

// DiskGuard 在大块写入时检查磁盘剩余空间：第一次写之前检查一次 Dir 所在的磁盘，之后每写 DiskCheckEvery 字节检查一次。
// 不能并发使用。
type DiskGuard struct {
	Dir     string
	written int64 // 上次检查以后写了多少
	checked bool
}

// Check 记下即将写入 n 字节，到了检查的时候检查磁盘。
func (g *DiskGuard) Check(n int) error {
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
