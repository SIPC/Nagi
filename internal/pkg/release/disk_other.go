//go:build !linux

package release

// 面板和节点只支持 Linux。其他系统（开发机）上读不到磁盘，不检查。
func statDisk(string) (total, free int64) { return 0, 0 }
