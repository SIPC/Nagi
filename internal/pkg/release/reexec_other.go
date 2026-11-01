//go:build !unix

package release

import "errors"

// 面板和节点只支持 Linux。
func Reexec(string) error { return errors.New("这个系统不支持原地重启") }
