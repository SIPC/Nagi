//go:build unix

package fileops

import "syscall"

const oNonblock = syscall.O_NONBLOCK
