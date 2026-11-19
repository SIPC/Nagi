package release

import (
	"errors"
	"io"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// 下载新版本时检查磁盘剩余空间，规则和节点的 metrics.DiskGuard 一样（剩余不到 2 GiB 或 5% 时停下，每写 64 MiB 查一次）。
// 面板也用这个包，所以不引用 internal/agent 下的包，自己读磁盘。
const (
	minFreeBytes   = 2 << 30
	minFreePercent = 5
	diskCheckEvery = 64 << 20
)

// diskSpace 读 path 所在磁盘的总量和可用量（读不到时都是 0），测试时替换。
var diskSpace = statDisk

// lowDisk 是磁盘剩余空间不够、停止下载的错误。所有下载地址都写同一块盘，遇到它不再换地址。
type lowDisk struct{ error }

func (e lowDisk) Unwrap() error { return e.error }

func isLowDisk(err error) bool {
	var l lowDisk
	return errors.As(err, &l)
}

// checkDisk 在 dir 所在磁盘剩余空间低于 minFreeBytes 或 minFreePercent 时返回 lowDisk。读不到时不拦。
func checkDisk(dir string) error {
	total, free := diskSpace(dir)
	if total <= 0 || free >= minFreeBytes && free*100 >= total*minFreePercent {
		return nil
	}
	return lowDisk{i18n.Errorf("磁盘剩余空间不足（剩余 %.1f GiB，共 %.1f GiB），已停止下载", float64(free)/(1<<30), float64(total)/(1<<30))}
}

// diskWriter 第一次写之前检查一次 dir 所在的磁盘，之后每写 diskCheckEvery 字节检查一次，再写进 w。
type diskWriter struct {
	dir     string
	w       io.Writer
	written int64
	checked bool
}

func (d *diskWriter) Write(p []byte) (int, error) {
	if !d.checked || d.written >= diskCheckEvery {
		if err := checkDisk(d.dir); err != nil {
			return 0, err
		}
		d.checked, d.written = true, 0
	}
	d.written += int64(len(p))
	return d.w.Write(p)
}
