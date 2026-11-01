//go:build !linux

package metrics

// 节点只支持 Linux。其他系统（开发机）上不采集节点指标。

// DiskSpace 返回 path 所在文件系统的总量和可用量，其他系统上都是 0。
func DiskSpace(string) (total, free int64) { return 0, 0 }

func readCPU() (total, idle uint64, ok bool)        { return 0, 0, false }
func readMem() (total, available uint64, ok bool)   { return 0, 0, false }
func readDisk(string) (total, free uint64, ok bool) { return 0, 0, false }
func readNet() (rx, tx uint64, ok bool)             { return 0, 0, false }
func mountOf(string) string                         { return "" }
func readMounts() []mountEntry                      { return nil }
