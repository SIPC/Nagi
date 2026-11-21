//go:build linux

package metrics

import "github.com/prometheus/procfs"

func pressure() float64 {
	fs, err := procfs.NewFS("/proc")
	if err != nil {
		return 0
	}
	var most float64
	for _, resource := range []string{"cpu", "memory", "io"} {
		if psi, err := fs.PSIStatsForResource(resource); err == nil && psi.Some != nil {
			most = max(most, psi.Some.Avg10)
		}
	}
	return most
}
