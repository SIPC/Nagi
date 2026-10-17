//go:build !linux

package metrics

// 节点只支持 Linux。其他系统（开发机）上不采集节点指标。

func readCPU() (total, idle uint64, ok bool)        { return 0, 0, false }
func readMem() (total, available uint64, ok bool)   { return 0, 0, false }
func readDisk(string) (total, free uint64, ok bool) { return 0, 0, false }
func readNet() (rx, tx uint64, ok bool)             { return 0, 0, false }
