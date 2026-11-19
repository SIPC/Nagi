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
	w := Window{Res: Interval, From: time.Unix(600, 0), To: time.Unix(660, 0)}
	// 不大于采样间隔时原样返回。
	if got := Aggregate(data, 10*time.Second, w); len(got["r:p/web:req"]) != 6 {
		t.Fatal("step 不大于采样间隔时不应聚合")
	}
	got := Aggregate(data, 30*time.Second, w)
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

// 速率序列（请求、连接、流量）合并时，窗口里没有采样的时间算 0；当前连接数、耗时和其他序列照常平均。
// 窗口只有一部分在数据覆盖的时间段里（查询的起点、现在、5 分钟精度还没降采样的部分）时，按覆盖的部分算。
func TestAggregateRates(t *testing.T) {
	at := func(ts int64, v float64) Point { return Point{Time: time.Unix(ts, 0), Value: v} }
	// 映射在 60 秒的窗口里只有最后 20 秒有采样：每秒 3 个连接，共 60 个。
	data := map[string][]Point{
		"r:p/db:conn":   {at(640, 3), at(650, 3)},
		"r:p/db:active": {at(640, 4), at(650, 2)},
		"r:p/db:lat":    {at(640, 10), at(650, 30)},
		"c:p/db/db:cpu": {at(640, 50), at(650, 70)},
	}
	got := Aggregate(data, time.Minute, Window{Res: Interval, From: time.Unix(0, 0), To: time.Unix(1000, 0)})
	for name, want := range map[string]float64{"r:p/db:conn": 1, "r:p/db:active": 3, "r:p/db:lat": 20, "c:p/db/db:cpu": 60} {
		if ps := got[name]; len(ps) != 1 || ps[0].Value != want || ps[0].Time.Unix() != 600 {
			t.Errorf("%s：%v，应为 %v @ 600", name, ps, want)
		}
	}
	// 窗口的后半段还没到（现在是 630）：只按已经过去的 30 秒算。
	got = Aggregate(map[string][]Point{"r:p/db:in": {at(610, 6), at(620, 6)}}, time.Minute, Window{Res: Interval, From: time.Unix(0, 0), To: time.Unix(630, 0)})
	if ps := got["r:p/db:in"]; len(ps) != 1 || ps[0].Value != 4 {
		t.Errorf("进行中的窗口：%v，应为 4", ps)
	}
	// 采样比应有的多（采样时间有抖动）时不放大，照常平均。
	got = Aggregate(map[string][]Point{"r:p/db:in": {at(600, 6), at(605, 6), at(610, 6), at(620, 6), at(630, 6), at(640, 6), at(650, 6)}}, time.Minute, Window{Res: Interval, From: time.Unix(0, 0), To: time.Unix(1000, 0)})
	if ps := got["r:p/db:in"]; len(ps) != 1 || ps[0].Value != 6 {
		t.Errorf("采样有抖动：%v，应为 6", ps)
	}
	// 5 分钟精度：10 分钟的窗口里只有一个 5 分钟的点。
	got = Aggregate(map[string][]Point{"r:p/db:req": {at(900, 2)}}, 10*time.Minute, Window{Res: 5 * time.Minute, From: time.Unix(0, 0), To: time.Unix(3600, 0)})
	if ps := got["r:p/db:req"]; len(ps) != 1 || ps[0].Value != 1 {
		t.Errorf("5 分钟精度：%v，应为 1", ps)
	}
}

func TestRateSeries(t *testing.T) {
	for name, want := range map[string]bool{
		"r:p/web:req": true, "r:p/web:5xx": true, "r:p/web:in": true, "r:p/db:conn": true, "r:p/db:fail": true,
		"r:p/db:active": false, "r:p/web:lat": false, "r:p/web:p95": false, "node:net_rx": false, "c:p/web/web:net_rx": false,
		// 映射名里带冒号以外的字符不影响，只看最后一段。
		"r:p/req-in:out": true,
	} {
		if rateSeries(name) != want {
			t.Errorf("rateSeries(%q) 应为 %v", name, want)
		}
	}
}
