package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestRawMetricHistoryKeepsFullChunksAndDoesNotInventMissingValues(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	cfg := logstore.DefaultConfig()
	cfg.MetricsEverySeconds = 20
	a := &agent{cfg: Config{Logs: logs, LogConfig: cfg}}
	collect := a.logMetrics()
	start := time.Now().Add(-48 * time.Hour) // 新库不套用旧图表的 24 小时降采样规则。
	values := map[string]float64{"node:cpu": 37.5, "bad": math.NaN(), "c:project/secret-service:mem": 12345}
	for i := range 67 {
		values[fmt.Sprintf("c:project/container/service-%02d:mem", i)] = float64(1000 + i)
	}
	collect(start, values)
	collect(start.Add(10*time.Second), map[string]float64{"node:cpu": 99}) // 入库频率可配，不篡改采样频率。
	collect(start.Add(20*time.Second), map[string]float64{"node:cpu": 42})
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Metrics})
	if err != nil || len(rows) != 4 {
		t.Fatalf("原始采样没有分块完整入库: rows=%d err=%v", len(rows), err)
	}
	seen := map[string]float64{}
	for _, row := range rows[:3] {
		if !row.Time.Equal(start) {
			t.Fatalf("旧时间被改成了当前时间: %+v", row)
		}
		var data struct {
			Values []struct {
				Series string
				Value  float64
			}
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		for _, p := range data.Values {
			seen[p.Series] = p.Value
		}
	}
	if len(seen) != 69 || seen["node:cpu"] != 37.5 || seen["c:project/container/service-66:mem"] != 1066 || seen["c:project/secret-service:mem"] != 12345 {
		t.Fatalf("分块边界丢失/错误补零/把非有限数当正常采样: %+v", seen)
	}
	if _, exists := seen["bad"]; exists {
		t.Fatal("缺失值被补造")
	}
	invalid, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.System})
	if err != nil || len(invalid) != 1 || invalid[0].Kind != "sample.invalid" {
		t.Fatalf("无效数值没有留痕: %+v err=%v", invalid, err)
	}
}
