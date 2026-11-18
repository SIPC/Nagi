package mcp

import (
	"math"
	"testing"
)

// 下载百分比夹在 0–100：节点报的已下载比总大小多、接近 int64 上限时不能超过 100 或溢出成负数。
func TestDownloadPercent(t *testing.T) {
	for _, c := range []struct{ got, total, want int64 }{
		{5, 10, 50},
		{29, 100, 29},
		{0, 10, 0},
		{-5, 10, 0},
		{10, 0, 0},
		{20, 10, 100},
		{math.MaxInt64, 10, 100},
		{math.MaxInt64 / 2, math.MaxInt64, 50},
	} {
		if p := downloadPercent(c.got, c.total); p != c.want {
			t.Errorf("downloadPercent(%d, %d) = %d，应为 %d", c.got, c.total, p, c.want)
		}
	}
}
