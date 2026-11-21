//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPressurePrefersOwnCgroup(t *testing.T) {
	root := t.TempDir()
	cg, proc := filepath.Join(root, "cg"), filepath.Join(root, "proc")
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	psi := func(some, full string) string {
		return "some avg10=" + some + " avg60=0 avg300=0 total=1\nfull avg10=" + full + " avg60=0 avg300=0 total=1\n"
	}
	// The host is saturated; this service's own cgroup is not.
	for _, r := range []string{"cpu", "memory", "io"} {
		write(filepath.Join(proc, "pressure", r), psi("90", "90"))
	}
	write(filepath.Join(proc, "self", "cgroup"), "0::/system.slice/nagi.service/leaf\n")
	svc := filepath.Join(cg, "system.slice", "nagi.service")
	write(filepath.Join(svc, "cpu.pressure"), psi("12", "3"))
	// I/O "some" counts one busy writer; only "full" means the service stalls.
	write(filepath.Join(svc, "io.pressure"), psi("70", "25"))
	write(filepath.Join(svc, "memory.pressure"), psi("5", "1"))
	if got := pressureIn(cg, proc); got != 25 {
		t.Fatalf("pressure = %v, want 25 (own cgroup's io full, not host or io some)", got)
	}
	// Without cgroup PSI, the host figures are the only evidence.
	if got := pressureIn(filepath.Join(root, "none"), proc); got != 90 {
		t.Fatalf("fallback pressure = %v, want 90", got)
	}
}