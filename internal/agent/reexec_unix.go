//go:build unix

package agent

import (
	"os"
	"syscall"
)

// reexec 用新的可执行文件替换当前进程（进程号不变）。
func reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
