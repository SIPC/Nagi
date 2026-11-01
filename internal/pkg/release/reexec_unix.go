//go:build unix

package release

import (
	"os"
	"syscall"
)

// Reexec 用新的可执行文件替换当前进程（进程号不变，systemd 和容器里都适用）。
func Reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
