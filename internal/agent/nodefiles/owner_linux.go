//go:build linux

package nodefiles

import (
	"io/fs"
	"syscall"
)

func fileOwner(info fs.FileInfo) (uid, gid int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
