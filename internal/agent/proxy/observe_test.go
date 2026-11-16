package proxy

import (
	"fmt"
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
	if p := s.Percentile(0.99); p > 0.31 {
		t.Errorf("P99 = %g，被 WebSocket 拉高了", p)
	}

	// 只有 WebSocket 时没有耗时。
	o.record(fmt.Appendf(nil, `{"nagi_route":"p/ws","status":101,"duration":300}`))
	if s := o.stats["p/ws"]; s.Requests != 1 || s.Timed != 0 || len(s.Durations) != 0 {
		t.Errorf("只有 WebSocket：%+v", s)
	}
}
