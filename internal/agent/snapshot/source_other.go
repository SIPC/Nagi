//go:build !linux

package snapshot

import (
	"os"

	"github.com/kopia/kopia/fs"
)

// oOpenFlags 在其他系统（开发机）上不用：节点只支持 Linux，打开后照样 fstat 核对。
const oOpenFlags = 0

func ownerInfo(os.FileInfo) fs.OwnerInfo   { return fs.OwnerInfo{} }
func deviceInfo(os.FileInfo) fs.DeviceInfo { return fs.DeviceInfo{} }
