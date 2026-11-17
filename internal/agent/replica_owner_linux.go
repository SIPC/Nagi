//go:build linux

package agent

import (
	"io/fs"
	"syscall"
)

// fileUID 返回文件的属主。
func fileUID(info fs.FileInfo) (int, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}
	return 0, false
}
