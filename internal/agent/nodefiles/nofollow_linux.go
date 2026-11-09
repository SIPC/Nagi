//go:build linux

package nodefiles

import "syscall"

// oNoFollow 让打开时不跟随最后一段的符号链接。
const oNoFollow = syscall.O_NOFOLLOW
