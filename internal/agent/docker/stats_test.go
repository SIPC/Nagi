package docker

import (
	"fmt"
	"net/http"
	"testing"
)

func TestContainerStatsKeepsMissingDetailsDistinctFromZero(t *testing.T) {
	response := `{"cpu_stats":{"cpu_usage":{"total_usage":123,"usage_in_usermode":41,"percpu_usage":[11,112]},"system_cpu_usage":999,"online_cpus":2,"throttling_data":{"periods":20,"throttled_periods":0,"throttled_time":789}},"memory_stats":{"usage":1000,"limit":9000,"stats":{"inactive_file":300}},"pids_stats":{"current":3},"networks":{"eth0":{"rx_bytes":13,"tx_bytes":29,"rx_errors":0,"endpoint_id":"not a number"}},"blkio_stats":{"io_service_bytes_recursive":[{"major":8,"minor":1,"op":"Read","value":512},{"major":8,"minor":1,"op":"Write","value":1536},{"major":8,"minor":1,"op":"Total","value":2048}]}}`
	d := historyClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
	stats, err := d.ContainerStats(t.Context(), "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if stats.MemUsed != 700 || stats.NetRx != 13 || stats.NetTx != 29 || stats.CPUTotal != 123 {
		t.Fatalf("existing aggregate changed: %+v", stats)
	}
	for key, want := range map[string]float64{"pids": 3, "cpu_throttled_periods_total": 0, "cpu_throttled_nanoseconds_total": 789,
		"memory_usage_bytes": 1000, "interface@eth0:tx_bytes_total": 29, "interface@eth0:rx_errors_total": 0,
		"device@8.1:io_bytes_read_total": 512, "device@8.1:io_bytes_write_total": 1536} {
		if got, present := stats.Details[key]; !present || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, present, want)
		}
	}
	for _, key := range []string{"memory_peak_bytes", "memory_limit_hits_total", "pids_limit", "device@8.1:io_ops_read_total", "interface@eth0:rx_dropped_total"} {
		if _, present := stats.Details[key]; present {
			t.Errorf("unmeasured feature %s must not become zero", key)
		}
	}
	response = `{"cpu_stats":{},"memory_stats":{},"pids_stats":{},"networks":null,"blkio_stats":{"io_service_bytes_recursive":null}}`
	stats, err = d.ContainerStats(t.Context(), "instance-a")
	if err != nil || len(stats.Details) != 0 {
		t.Fatalf("empty old Docker response must have no measured details: %+v %v", stats, err)
	}
}
