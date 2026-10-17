package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
)

// 这里注册两个 Caddy 模块，把 Caddy 内部的信息交给节点：
//   - 日志输出 nagi_metrics：读访问日志，按规则统计请求数、5xx 数和耗时；
//   - 事件处理 nagi：证书申请成功或失败时通知节点，用于告警。
//
// Caddy 模块由 Caddy 自己创建，拿不到 Proxy，所以通过包级变量 observer 传递。

func init() {
	caddy.RegisterModule(metricsWriter{})
	caddy.RegisterModule(certEvents{})
}

// logRouteKey 是每条 http 规则在访问日志里追加的字段，值为 "<项目 ID>/<规则名>"。
const logRouteKey = "nagi_route"

// RouteStats 是一条规则在一段时间内的请求统计。
type RouteStats struct {
	Requests int
	Errors   int     // 5xx 响应数
	Seconds  float64 // 耗时总和
}

type observerT struct {
	mu     sync.Mutex
	stats  map[string]*RouteStats
	onCert func(domain string, err error)
}

var observer = &observerT{stats: map[string]*RouteStats{}}

// TakeStats 返回上次调用以来每条规则的请求统计，键是 "<项目 ID>/<规则名>"。
func TakeStats() map[string]RouteStats {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	out := make(map[string]RouteStats, len(observer.stats))
	for k, v := range observer.stats {
		out[k] = *v
	}
	observer.stats = map[string]*RouteStats{}
	return out
}

// OnCertEvent 设置证书事件的回调：申请成功时 err 为 nil。
func OnCertEvent(fn func(domain string, err error)) {
	observer.mu.Lock()
	observer.onCert = fn
	observer.mu.Unlock()
}

func (o *observerT) record(line []byte) {
	var entry struct {
		Route    string  `json:"nagi_route"`
		Status   int     `json:"status"`
		Duration float64 `json:"duration"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Route == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.stats[entry.Route]
	if s == nil {
		s = &RouteStats{}
		o.stats[entry.Route] = s
	}
	s.Requests++
	if entry.Status >= 500 {
		s.Errors++
	}
	s.Seconds += entry.Duration
}

// ---- 访问日志 → 统计 ----

type metricsWriter struct{}

func (metricsWriter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.logging.writers.nagi_metrics",
		New: func() caddy.Module { return new(metricsWriter) },
	}
}

func (metricsWriter) String() string    { return "nagi_metrics" }
func (metricsWriter) WriterKey() string { return "nagi_metrics" }
func (metricsWriter) OpenWriter() (io.WriteCloser, error) {
	return &lineWriter{}, nil
}

// lineWriter 按行解析 JSON 日志。
type lineWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		observer.record(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 1<<20 {
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) Close() error { return nil }

// ---- 证书事件 ----

type certEvents struct{}

func (certEvents) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "events.handlers.nagi",
		New: func() caddy.Module { return new(certEvents) },
	}
}

func (certEvents) Handle(_ context.Context, e caddy.Event) error {
	observer.mu.Lock()
	fn := observer.onCert
	observer.mu.Unlock()
	if fn == nil {
		return nil
	}
	domain, _ := e.Data["identifier"].(string)
	switch e.Name() {
	case "cert_obtained":
		fn(domain, nil)
	case "cert_failed":
		err, _ := e.Data["error"].(error)
		if err == nil {
			err = fmt.Errorf("%v", e.Data["error"])
		}
		fn(domain, err)
	}
	return nil
}

var (
	_ caddy.WriterOpener  = (*metricsWriter)(nil)
	_ caddyevents.Handler = (*certEvents)(nil)
)
