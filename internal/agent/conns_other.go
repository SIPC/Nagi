//go:build !linux

package agent

// connsSupported：网络连接视图只在 Linux 上有（sock_diag、/proc）。
const connsSupported = false

func newAgentConns(*agent) *connWatch { return nil }
