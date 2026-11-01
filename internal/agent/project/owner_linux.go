package project

import (
	"io/fs"
	"syscall"
)

// fileOwner 返回文件的属主，取不到时返回 -1。
func fileOwner(info fs.FileInfo) (uid, gid int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
