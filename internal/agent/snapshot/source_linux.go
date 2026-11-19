//go:build linux

package snapshot

import (
	"os"
	"syscall"

	"github.com/kopia/kopia/fs"
)

// oOpenFlags：打开来源里的文件和目录时不跟随最后一段的符号链接，也不阻塞（容器换上的 FIFO 不会卡住 open(2)）。
const oOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func ownerInfo(fi os.FileInfo) fs.OwnerInfo {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fs.OwnerInfo{UserID: st.Uid, GroupID: st.Gid}
	}
	return fs.OwnerInfo{}
}

func deviceInfo(fi os.FileInfo) fs.DeviceInfo {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fs.DeviceInfo{Dev: uint64(st.Dev), Rdev: uint64(st.Rdev)} //nolint:unconvert // 部分架构上不是 uint64
	}
	return fs.DeviceInfo{}
}
