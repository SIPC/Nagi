package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
)

// 这里注册两个 Caddy 模块，把 Caddy 内部的信息交给节点：
//   - 日志输出 nagi_metrics：读访问日志，按规则统计请求数、各类状态码、收发字节和耗时；
//   - 应用 nagi_cert_events：证书申请成功或失败时通知节点，用于告警。
//
// Caddy 模块由 Caddy 自己创建，拿不到 Proxy，所以通过包级变量 observer 传递。

func init() {
	caddy.RegisterModule(metricsWriter{})
	caddy.RegisterModule(certEvents{})
}

// logRouteKey 是每条 http 规则在访问日志里追加的字段，值为 "<项目 ID>/<规则名>"。
const logRouteKey = "nagi_route"

// RouteStats 是一条规则在一段时间内的请求统计。http 规则记请求，tcp/udp 规则（L4）记连接（见 l4stats.go）。
type RouteStats struct {
	Requests int
	Errors   int // 5xx 响应数
	// Timed 是计入耗时的请求数，Seconds 是它们的耗时总和。WebSocket（101 切换协议）不计入：它的耗时是整条连接开着的
	// 时间，动辄几分钟，算进去平均值和分位数就没有意义了。
	Timed   int
	Seconds float64
	// Classes 是各类状态码的响应数，下标是状态码的百位（1xx～5xx）。
	Classes  [6]int
	Statuses map[int]int // 具体 HTTP 状态码；只收 100～599，避免无界标签。
	Maximum  float64     // 全部非 WebSocket 请求的最大耗时，不从抽样估算。
	BytesIn  int64       // 请求体字节数；tcp/udp 是从客户端收到的字节数
	BytesOut int64       // 响应体字节数；tcp/udp 是发给客户端的字节数
	// L4 表示这是 tcp/udp 规则：Conns 是新连接数（UDP 按会话），Fails 是其中连不上容器的，Active 是取统计时的连接数。
	L4     bool
	Conns  int
	Fails  int
	Active int
	// Durations 是耗时的样本（秒），用来算分位数。请求太多时只留 maxSamples 个，等概率抽样。
	Durations []float64
}

// maxSamples 是一条规则在一个采样间隔内最多保留的耗时样本数。
const maxSamples = 2048

// Percentile 返回耗时样本的第 q 分位（0~1，最近秩法），单位秒。没有样本时返回 0。
// 会对 Durations 排序。
func (s *RouteStats) Percentile(q float64) float64 {
	if len(s.Durations) == 0 {
		return 0
	}
	slices.Sort(s.Durations)
	i := int(q*float64(len(s.Durations))+0.5) - 1
	return s.Durations[min(max(i, 0), len(s.Durations)-1)]
}

type observerT struct {
	mu           sync.Mutex
	stats        map[string]*RouteStats
	l4           map[string]*l4Counter
	onCert       func(domain string, err error)
	onAccess     func(AccessRecord)
	onConnection func(ConnectionRecord)
}

var observer = &observerT{stats: map[string]*RouteStats{}, l4: map[string]*l4Counter{}}

// TakeStats 返回上次调用以来每条规则的请求统计，键是 "<项目 ID>/<规则名>"。
func TakeStats() map[string]RouteStats {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	out := make(map[string]RouteStats, len(observer.stats))
	for k, v := range observer.stats {
		out[k] = *v
	}
	observer.stats = map[string]*RouteStats{}
	observer.takeL4(out)
	return out
}

// OnCertEvent 设置证书事件的回调：申请成功时 err 为 nil。
func OnCertEvent(fn func(domain string, err error)) {
	observer.mu.Lock()
	observer.onCert = fn
	observer.mu.Unlock()
}

// AccessRecord 只取访问日志的白名单字段，不把 Caddy 的请求头/凭据原文交给日志库。
// PathHash 用于关联同一路径；路径本身暂不记录，避免任意业务路径内的秘密泄露。
type AccessRecord struct {
	Time      time.Time `json:"time"`
	Route     string    `json:"route"`
	Status    int       `json:"status"`
	Duration  float64   `json:"duration_seconds"`
	BytesRead int64     `json:"bytes_in"`
	Size      int64     `json:"bytes_out"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Remote    string    `json:"remote"`
	Protocol  string    `json:"protocol"`
	PathHash  string    `json:"path_hash"`
}

// OnAccess 包括没有匹配业务规则的请求；不会改变已有告警聚合只统计业务规则的行为。
func OnAccess(fn func(AccessRecord)) {
	observer.mu.Lock()
	observer.onAccess = fn
	observer.mu.Unlock()
}

func (o *observerT) record(line []byte) {
	var entry struct {
		Time      float64 `json:"ts"`
		Route     string  `json:"nagi_route"`
		Status    int     `json:"status"`
		Duration  float64 `json:"duration"`
		BytesRead int64   `json:"bytes_read"`
		Size      int64   `json:"size"`
		Request   struct {
			Method   string `json:"method"`
			Host     string `json:"host"`
			Remote   string `json:"remote_ip"`
			Protocol string `json:"proto"`
			URI      string `json:"uri"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return
	}
	o.mu.Lock()
	fn := o.onAccess
	o.mu.Unlock()
	if fn != nil {
		e := AccessRecord{Time: time.Unix(0, int64(entry.Time*1e9)), Route: entry.Route, Status: entry.Status,
			Duration: entry.Duration, BytesRead: entry.BytesRead, Size: entry.Size,
			Method: entry.Request.Method, Host: entry.Request.Host, Remote: entry.Request.Remote, Protocol: entry.Request.Protocol}
		if entry.Time == 0 {
			e.Time = time.Now()
		}
		if uri, err := url.ParseRequestURI(entry.Request.URI); err == nil {
			e.PathHash = fmt.Sprintf("%x", sha256.Sum256([]byte(uri.Path)))
		}
		fn(e)
	}
	if entry.Route == "" {
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
	if c := entry.Status / 100; c >= 1 && c <= 5 {
		s.Classes[c]++
		if s.Statuses == nil {
			s.Statuses = map[int]int{}
		}
		s.Statuses[entry.Status]++
	}
	s.BytesIn += entry.BytesRead
	s.BytesOut += entry.Size
	if entry.Status == http.StatusSwitchingProtocols {
		return
	}
	s.Timed++
	s.Seconds += entry.Duration
	s.Maximum = max(s.Maximum, entry.Duration)
	// 蓄水池抽样：每个请求被留下的概率相同，内存有上限。
	if len(s.Durations) < maxSamples {
		s.Durations = append(s.Durations, entry.Duration)
	} else if i := rand.IntN(s.Timed); i < maxSamples {
		s.Durations[i] = entry.Duration
	}
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
		ID:  "nagi_cert_events",
		New: func() caddy.Module { return new(certEvents) },
	}
}

func (h *certEvents) Provision(ctx caddy.Context) error {
	app, err := ctx.App("events")
	if err != nil {
		return err
	}
	// Caddy starts apps in arbitrary order. Subscribe before any app starts:
	// TLS can emit events from background certificate jobs as soon as it starts.
	return app.(*caddyevents.App).Subscribe(&caddyevents.Subscription{
		Events:   []string{"cert_obtained", "cert_failed"},
		Handlers: []caddyevents.Handler{h},
	})
}

func (*certEvents) Start() error { return nil }
func (*certEvents) Stop() error  { return nil }

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
	_ caddy.App           = (*certEvents)(nil)
	_ caddy.Provisioner   = (*certEvents)(nil)
	_ caddyevents.Handler = (*certEvents)(nil)
)
