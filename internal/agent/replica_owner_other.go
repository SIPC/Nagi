//go:build !linux

package agent

import "io/fs"

// fileUID 在非 Linux 上不检查属主（节点只在 Linux 上运行）。
func fileUID(fs.FileInfo) (int, bool) { return 0, false }
