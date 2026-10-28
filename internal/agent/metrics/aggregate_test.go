package metrics

import (
	"testing"
	"time"
)

func TestAggregate(t *testing.T) {
	pts := func(start int64, values ...float64) []Point {
		var out []Point
		for i, v := range values {
			out = append(out, Point{Time: time.Unix(start+int64(i)*10, 0), Value: v})
		}
		return out
	}
	data := map[string][]Point{
		"r:p/web:req": pts(600, 1, 2, 3, 6, 4, 2),
		"r:p/web:p95": pts(600, 10, 50, 20, 5, 80, 30),
	}
	// 不大于采样间隔时原样返回。
	if got := Aggregate(data, 10*time.Second); len(got["r:p/web:req"]) != 6 {
		t.Fatal("step 不大于采样间隔时不应聚合")
	}
	got := Aggregate(data, 30*time.Second)
	check := func(name string, want ...float64) {
		t.Helper()
		ps := got[name]
		if len(ps) != len(want) {
			t.Fatalf("%s：得到 %d 个点，应为 %d 个", name, len(ps), len(want))
		}
		for i, p := range ps {
			if p.Value != want[i] || p.Time.Unix() != 600+int64(i)*30 {
				t.Errorf("%s 第 %d 个点：%v @ %d，应为 %v @ %d", name, i, p.Value, p.Time.Unix(), want[i], 600+int64(i)*30)
			}
		}
	}
	check("r:p/web:req", 2, 4)   // 平均值
	check("r:p/web:p95", 50, 80) // 最大值
}
