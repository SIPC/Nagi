//go:build !unix

package agent

import "errors"

// 节点只支持 Linux。
func reexec(string) error { return errors.New("这个系统不支持原地重启") }
