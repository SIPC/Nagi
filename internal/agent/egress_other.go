//go:build !linux

package agent

import "context"

// 出站限制只在 Linux 上有（nftables），别的系统上不限制（开发用）。
type egress struct{ a *agent }

func newEgress(a *agent) *egress { return &egress{a: a} }

func (e *egress) Error() string { return "" }

func (e *egress) trigger() {}

func (e *egress) run(ctx context.Context) {}
