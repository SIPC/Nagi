package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// WebSocket（101）计入请求数和流量，不计入耗时：它的耗时是整条连接开着的时间。
func TestObserveSkipsWebSocketDuration(t *testing.T) {
	o := &observerT{stats: map[string]*RouteStats{}}
	line := func(status int, duration float64) []byte {
		return fmt.Appendf(nil, `{"nagi_route":"p/web","status":%d,"duration":%g,"bytes_read":10,"size":20}`, status, duration)
	}
	o.record(line(200, 0.1))
	o.record(line(200, 0.3))
	o.record(line(101, 600))
	s := o.stats["p/web"]
	if s.Requests != 3 || s.Classes[1] != 1 || s.BytesIn != 30 || s.BytesOut != 60 {
		t.Errorf("请求数和流量要包括 WebSocket：%+v", s)
	}
	if s.Timed != 2 || s.Seconds < 0.39 || s.Seconds > 0.41 || len(s.Durations) != 2 {
		t.Errorf("耗时不能包括 WebSocket：timed=%d seconds=%g samples=%v", s.Timed, s.Seconds, s.Durations)
	}
	if s.Statuses[200] != 2 || s.Statuses[101] != 1 || s.Maximum != 0.3 {
		t.Fatalf("具体状态或最大耗时丢失: %+v", s)
	}
	if p := s.Percentile(0.99); p > 0.31 {
		t.Errorf("P99 = %g，被 WebSocket 拉高了", p)
	}

	// 只有 WebSocket 时没有耗时。
	o.record(fmt.Appendf(nil, `{"nagi_route":"p/ws","status":101,"duration":300}`))
	if s := o.stats["p/ws"]; s.Requests != 1 || s.Timed != 0 || len(s.Durations) != 0 {
		t.Errorf("只有 WebSocket：%+v", s)
	}
	for range maxSamples + 13 {
		o.record(line(429, 0.2))
	}
	o.record(line(503, 3.75))
	o.record(line(99999, 0.1))
	if s.Statuses[429] != maxSamples+13 || s.Statuses[503] != 1 || len(s.Statuses) != 4 || s.Maximum != 3.75 || len(s.Durations) != maxSamples {
		t.Fatalf("抽样影响了完整计数/状态码维度没有界限: %+v", s)
	}
}

func TestAccessRecordsUnmatchedRequestsWithoutChangingRuleStatistics(t *testing.T) {
	var records []AccessRecord
	OnAccess(func(e AccessRecord) { records = append(records, e) })
	defer OnAccess(nil)
	w := &lineWriter{}
	for _, line := range []string{
		`{"status":404,"duration":0.025,"bytes_read":3,"size":19,"request":{"method":"GET","host":"example.test","uri":"/private-value?token=credential","remote_ip":"192.0.2.3","headers":{"Authorization":["Bearer raw-secret"]}}}`,
		`{"nagi_route":"p/web","status":503,"duration":0.125,"bytes_read":7,"size":23}`,
	} {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if len(records) != 2 || records[0].Status != 404 || records[0].Duration != 0.025 || records[0].BytesRead != 3 || records[1].Route != "p/web" || records[1].Size != 23 {
		t.Fatalf("逐请求事实丢失: %+v", records)
	}
	b, _ := json.Marshal(records)
	for _, secret := range []string{"private-value", "credential", "raw-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("不应向日志库暴露原始路径/查询串/凭据: %s", b)
		}
	}
	stats := TakeStats()
	if stats[""].Requests != 0 || stats["p/web"].Requests != 1 || stats["p/web"].Errors != 1 {
		t.Fatalf("访问采集改变已有业务规则计数: %+v", stats)
	}
}
