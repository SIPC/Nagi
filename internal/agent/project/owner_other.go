//go:build !linux

package project

import "io/fs"

// fileOwner 在其他系统（开发机）上取不到属主。
func fileOwner(fs.FileInfo) (uid, gid int) { return -1, -1 }
