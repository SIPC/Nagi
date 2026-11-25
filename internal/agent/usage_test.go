package agent

import (
	"testing"

	"github.com/SIPC/nagi/internal/agent/metrics"
)

// 节点列表、概览的「读写」用上报里的 disk_read、disk_write：采到的 0 要带上，没采到的不能变成 0。
func TestNodeUsageReportsDiskIOOnlyWhenMeasured(t *testing.T) {
	u := nodeUsage(map[string]float64{"node:cpu": 12, "node:disk_read": 0, "node:disk_write": 4096}, []metrics.Disk{{Mount: "/", Total: 10, Used: 4}})
	if u.DiskRead == nil || *u.DiskRead != 0 || u.DiskWrite == nil || *u.DiskWrite != 4096 || u.Cpu != 12 || len(u.Disks) != 1 || u.Disks[0].UsedBytes != 4 {
		t.Fatalf("measured usage: %+v", u)
	}
	u = nodeUsage(map[string]float64{"node:cpu": 12, "node:disk_read": 5}, nil)
	if u.DiskRead == nil || *u.DiskRead != 5 || u.DiskWrite != nil {
		t.Fatalf("missing write must stay absent: %+v", u)
	}
	if u := nodeUsage(map[string]float64{}, nil); u.DiskRead != nil || u.DiskWrite != nil {
		t.Fatalf("no sample yet must not report zero disk I/O: %+v", u)
	}
}
