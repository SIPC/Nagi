//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeDetailsEvidenceAndCounterGaps(t *testing.T) {
	root, sys := t.TempDir(), t.TempDir()
	write := func(name, data string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu 100 0 50 850 0 0 0 0 20 0\ncpu2 100 0 50 850 0 0 0 0 20 0\nctxt 400\nprocesses 12\nprocs_running 3\nprocs_blocked 2\nbtime 123\n")
	write("meminfo", "MemTotal: 8192 kB\nMemAvailable: 3072 kB\nCached: 257 kB\nSwapTotal: 4096 kB\nSwapFree: 512 kB\n")
	write("loadavg", "1.5 2.25 3.75 3/55 42\n")
	write("pressure/cpu", "some avg10=1.25 avg60=2.50 avg300=3.75 total=123456\n")
	write("net/dev", "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n eth0: 1000 20 3 4 0 0 0 0 7000 60 7 8 0 0 0 0\n")
	write("diskstats", "8 0 sda 3 0 7 11 13 0 17 19 2 23 29\n")
	source := nodeDetails(root, sys)
	first := source(10)
	for key, want := range map[string]float64{"node:cpu@2:user_seconds_total": 1, "node:cpu_cores": 1,
		"node:memory:cached_bytes": 257 * 1024, "node:memory:swap_free_bytes": 512 * 1024,
		"node:load5": 2.25, "node:pressure@cpu:some_microseconds_total": 123456,
		"node:interface@eth0:tx_errors_total": 7, "node:device@sda:read_bytes_total": 7 * 512,
		"node:device@sda:write_milliseconds_total": 19, "node:device@sda:in_progress": 2,
		"node:source@network:ok": 1, "node:source@pressure.memory:ok": 0} {
		if got, present := first[key]; !present || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, present, want)
		}
	}
	for _, key := range []string{"node:memory:dirty_bytes", "node:interface@eth0:rx_bytes_per_second", "node:cpu@2:user_percent", "node:pressure@memory:some_avg10_percent"} {
		if _, present := first[key]; present {
			t.Errorf("unavailable observation %s must not appear as a measurement", key)
		}
	}
	write("stat", "cpu 130 0 60 910 0 0 0 0 30 0\ncpu2 130 0 60 910 0 0 0 0 30 0\n")
	write("net/dev", "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n eth0: 1250 25 3 4 0 0 0 0 7750 60 7 8 0 0 0 0\n")
	next := source(5)
	if got := next["node:interface@eth0:rx_bytes_per_second"]; got != 50 {
		t.Errorf("receive rate %v, want 50", got)
	}
	// Guest overlaps user. The CPU denominator is 100 ticks, not 110.
	if got := next["node:cpu@2:user_percent"]; got < 29.999 || got > 30.001 {
		t.Errorf("user CPU %v, want 30", got)
	}
	write("net/dev", "corrupt")
	missing := source(5)
	if missing["node:source@network:ok"] != 0 {
		t.Fatal("malformed source must be marked unavailable")
	}
	write("net/dev", "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n eth0: 5000 25 3 4 0 0 0 0 9000 60 7 8 0 0 0 0\n")
	if _, present := source(5)["node:interface@eth0:rx_bytes_per_second"]; present {
		t.Fatal("gap must invalidate the rate baseline")
	}
	write("net/dev", "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n eth0: 2 1 0 0 0 0 0 0 3 1 0 0 0 0 0 0\n")
	if _, present := source(5)["node:interface@eth0:rx_bytes_per_second"]; present {
		t.Fatal("counter reset must not produce a negative or wrapped rate")
	}
}

func TestNodeDetailsOnLinux(t *testing.T) {
	v := NodeDetails()(10)
	for _, key := range []string{"node:source@procfs:ok", "node:source@cpu:ok", "node:source@memory:ok", "node:source@network:ok", "node:source@self.stat:ok"} {
		if v[key] != 1 {
			t.Errorf("live source unavailable: %s", key)
		}
	}
	if v["node:memory:total_bytes"] <= 0 || v["node:nagi:rss_bytes"] <= 0 || v["node:nagi:goroutines"] <= 0 {
		t.Fatalf("missing live memory/process measurements: %v", v)
	}
}
