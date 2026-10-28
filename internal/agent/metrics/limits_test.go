package metrics

import (
	"testing"

	"github.com/SIPC/nagi/internal/agent/docker"
)

func TestLimitSum(t *testing.T) {
	const gb = 1 << 30
	host := docker.Stats{OnlineCPUs: 4, MemLimit: 8 * gb}
	cases := []struct {
		name     string
		services []docker.Runtime
		memTotal uint64
		cpu, mem float64
	}{
		{"没设限制按主机", []docker.Runtime{{}}, 8 * gb, 400, 8 * gb},
		{"设了限制按限制", []docker.Runtime{{CPULimit: 0.5, MemLimit: gb}}, 8 * gb, 50, gb},
		{"容器组加起来", []docker.Runtime{{CPULimit: 1, MemLimit: gb}, {CPULimit: 0.5, MemLimit: 2 * gb}}, 8 * gb, 150, 3 * gb},
		{"组里有一个没设就按主机", []docker.Runtime{{CPULimit: 1, MemLimit: gb}, {}}, 8 * gb, 400, 8 * gb},
		{"加起来超过主机按主机", []docker.Runtime{{CPULimit: 3, MemLimit: 6 * gb}, {CPULimit: 3, MemLimit: 6 * gb}}, 8 * gb, 400, 8 * gb},
		{"读不到主机内存时用 Docker 报的上限", []docker.Runtime{{}}, 0, 400, 8 * gb},
	}
	for _, c := range cases {
		var l limitSum
		for _, rt := range c.services {
			l.add(rt, host)
		}
		cpu, mem := l.result(c.memTotal)
		if cpu != c.cpu || mem != c.mem {
			t.Errorf("%s：得到 cpu=%v mem=%v，应为 cpu=%v mem=%v", c.name, cpu, mem, c.cpu, c.mem)
		}
	}
}
