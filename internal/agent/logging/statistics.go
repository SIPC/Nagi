package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

type metricPoint struct {
	Series string  `json:"series"`
	Value  float64 `json:"value"`
}

type metricSample struct {
	Interval int           `json:"interval_seconds"`
	Values   []metricPoint `json:"values"`
	Points   int           `json:"points"`
	Chunks   int           `json:"chunks"`
}

// CaptureMetrics queues bounded chunks followed by a completion marker as one
// batch: readers never see a partly persisted sample as a complete healthy
// observation, and the sampler never waits for the disk. A batch that cannot
// be queued or written is counted as lost in the store's write failures.
func CaptureMetrics(logs *logstore.Store, cfg logstore.Config) func(time.Time, map[string]float64) {
	var last time.Time
	return func(at time.Time, values map[string]float64) {
		if !last.IsZero() && at.Sub(last) >= 0 && at.Sub(last) < time.Duration(cfg.MetricsEverySeconds)*time.Second {
			return
		}
		last = at
		var batch, invalid []logstore.Event
		chunk := make([]metricPoint, 0, 32)
		points, chunks := 0, 0
		flush := func() {
			if len(chunk) == 0 {
				return
			}
			batch = append(batch, logstore.Event{Time: at, Category: logstore.Metrics, Kind: "sample", Data: metricSample{Interval: cfg.MetricsEverySeconds, Values: chunk}})
			points += len(chunk)
			chunks++
			chunk = make([]metricPoint, 0, 32)
		}
		for series, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				invalid = append(invalid, logstore.Event{Time: at, Category: logstore.System, Kind: "sample.invalid", Data: map[string]any{"series": series}})
				continue
			}
			chunk = append(chunk, metricPoint{series, value})
			if len(chunk) == 32 {
				flush()
			}
		}
		flush()
		batch = append(batch, logstore.Event{Time: at, Category: logstore.Metrics, Kind: "sample.end", Data: metricSample{Interval: cfg.MetricsEverySeconds, Points: points, Chunks: chunks}})
		logs.ReportBatch(batch)
		logs.ReportBatch(invalid)
	}
}

// Statistics is the alert module's read-only view of local logging storage.
// Latest values require complete journal samples. Window queries use the indexed
// local history database, not a scan of a day's multi-gigabyte raw journal.
// Neither path fills missing observations from live values.
type Statistics struct {
	Store   *logstore.Store
	History *metrics.Collector
}

func (s Statistics) LatestFresh(now time.Time) map[string]float64 {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, latest, err := s.readSamples(ctx, "", now.Add(-3*metrics.Interval), now.Add(time.Nanosecond))
	if err != nil {
		return nil
	}
	out := map[string]float64{}
	for series, points := range data {
		if len(points) > 0 && points[len(points)-1].Time.Equal(latest) && points[len(points)-1].Value >= 0 {
			out[series] = points[len(points)-1].Value
		}
	}
	return out
}

func (s Statistics) Query(ctx context.Context, prefix string, from, to time.Time) (map[string][]metrics.Point, error) {
	if s.History == nil {
		return nil, fmt.Errorf("本机独立统计历史库不可用")
	}
	return s.History.Query(ctx, prefix, from, to)
}

func (s Statistics) readSamples(ctx context.Context, prefix string, from, to time.Time) (map[string][]metrics.Point, time.Time, error) {
	var latest time.Time
	if s.Store == nil {
		return nil, latest, fmt.Errorf("本机日志统计不可用")
	}
	type batch struct {
		points []metricPoint
		chunks int
		end    *metricSample
	}
	batches := map[time.Time]*batch{}
	// Start at the window on the time index: walking the whole metrics
	// category from the beginning every alert round held the shared log gate
	// long enough to time out live container writes.
	after, err := s.Store.FirstSeqSince(ctx, from)
	if err != nil {
		return nil, latest, err
	}
	var watermark int64
	for pages := 0; ; pages++ {
		if pages == 32 {
			return nil, latest, fmt.Errorf("日志统计窗口超过单次读取上限，不能视作完整证据")
		}
		page, err := s.Store.ReadPage(ctx, logstore.Filter{Category: logstore.Metrics, From: from, To: to, After: after, Limit: 1000})
		if err != nil {
			return nil, latest, err
		}
		if pages == 0 {
			watermark = page.State.Last
		}
		for _, event := range page.Events {
			if event.Seq > watermark || (event.Kind != "sample" && event.Kind != "sample.end") {
				continue
			}
			var sample metricSample
			if err := json.Unmarshal(event.Data.(json.RawMessage), &sample); err != nil {
				return nil, latest, err
			}
			if strings.HasPrefix(prefix, "r:") && time.Duration(sample.Interval)*time.Second != metrics.Interval {
				return nil, latest, fmt.Errorf("告警请求窗口需要完整的十秒采样，当前记录频率不满足")
			}
			b := batches[event.Time]
			if b == nil {
				b = &batch{}
				batches[event.Time] = b
			}
			if event.Kind == "sample.end" {
				if b.end != nil {
					return nil, latest, fmt.Errorf("日志统计有重复采样批次")
				}
				b.end = &sample
			} else {
				b.points = append(b.points, sample.Values...)
				b.chunks++
			}
		}
		after = page.Next
		if !page.More || after >= watermark {
			break
		}
	}
	out := map[string][]metrics.Point{}
	for at, b := range batches {
		if b.end == nil || b.chunks != b.end.Chunks || len(b.points) != b.end.Points {
			return nil, latest, fmt.Errorf("日志统计采样未完整落盘")
		}
		if at.After(latest) {
			latest = at
		}
		seen := map[string]bool{}
		for _, p := range b.points {
			if seen[p.Series] || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				return nil, latest, fmt.Errorf("日志统计数值无效或重复")
			}
			seen[p.Series] = true
			if strings.HasPrefix(p.Series, prefix) {
				out[p.Series] = append(out[p.Series], metrics.Point{Time: at, Value: p.Value})
			}
		}
	}
	for _, points := range out {
		sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
	}
	return out, latest, nil
}
