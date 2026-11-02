//go:build !linux

package nodefiles

import "io/fs"

func fileOwner(fs.FileInfo) (uid, gid int) { return -1, -1 }
