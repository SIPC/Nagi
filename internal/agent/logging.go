package agent

import (
	"math"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func (a *agent) logMetrics() func(time.Time, map[string]float64) {
	var last time.Time
	return func(at time.Time, values map[string]float64) {
		if !last.IsZero() && at.Sub(last) >= 0 && at.Sub(last) < time.Duration(a.cfg.LogConfig.MetricsEverySeconds)*time.Second {
			return
		}
		last = at
		// 分块保存完整原始数值，不让大节点一整批采样撞上单条正文上限后被截断。
		type point struct {
			Series string  `json:"series"`
			Value  float64 `json:"value"`
		}
		// 序列名是值而不是任意 JSON 字段名，避免 secret-service 等名字被凭据字段脱敏误删数值。
		chunk := make([]point, 0, 32)
		flush := func() {
			if len(chunk) == 0 {
				return
			}
			a.cfg.Logs.Report(logstore.Event{Time: at, Category: logstore.Metrics, Kind: "sample", Data: map[string]any{
				"interval_seconds": a.cfg.LogConfig.MetricsEverySeconds, "values": chunk,
			}})
			chunk = make([]point, 0, 32)
		}
		for series, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				a.cfg.Logs.Report(logstore.Event{Time: at, Category: logstore.System, Kind: "sample.invalid", Data: map[string]any{"series": series}})
				continue
			}
			chunk = append(chunk, point{Series: series, Value: value})
			if len(chunk) >= 32 {
				flush()
			}
		}
		flush()
	}
}

func (a *agent) logProxyAccess(e proxy.AccessRecord) {
	project, _, _ := strings.Cut(e.Route, "/")
	a.cfg.Logs.Report(logstore.Event{Time: e.Time, Category: logstore.Access, Kind: "proxy.http", Project: project, Object: e.Route, Data: e})
}
