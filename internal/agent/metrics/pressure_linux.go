//go:build linux

package metrics

import (
	"os"
	"strconv"
	"strings"
)

// Prefer this process's cgroup v2 PSI: inside a container or a systemd slice,
// /proc/pressure describes the whole host, so a busy neighbour would make
// Nagi yield on a machine whose own share is idle. CPU uses "some" (runnable
// tasks waiting for a CPU). Memory and I/O use "full" (every non-idle task
// stalled): one busy writer keeps I/O "some" high on a healthy machine.
func pressure() float64 { return pressureIn("/sys/fs/cgroup", "/proc") }

func pressureIn(cgroupRoot, procRoot string) float64 {
	// cgroup v2 has a single "0::<path>" line.
	group := cgroupRoot
	if b, err := os.ReadFile(procRoot + "/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if p, ok := strings.CutPrefix(line, "0::"); ok {
				group += strings.TrimRight(p, "/")
			}
		}
	}
	var most float64
	for _, resource := range []string{"cpu", "memory", "io"} {
		kind := "full"
		if resource == "cpu" {
			kind = "some"
		}
		// Walk up to the nearest cgroup that exposes PSI (a leaf such as
		// docker-in-docker's /init may not), then fall back to the host.
		var v float64
		ok := false
		for g := group; !ok && strings.HasPrefix(g, cgroupRoot); g = g[:max(strings.LastIndex(g, "/"), 0)] {
			v, ok = psiAvg10(g+"/"+resource+".pressure", kind)
		}
		if !ok {
			v, _ = psiAvg10(procRoot+"/pressure/"+resource, kind)
		}
		most = max(most, v)
	}
	return most
}

// psiAvg10 reads avg10 of the "some" or "full" line of a PSI file.
func psiAvg10(path, kind string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != kind {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				n, err := strconv.ParseFloat(v, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}
