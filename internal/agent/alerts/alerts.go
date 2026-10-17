// Package alerts 在节点本地判断告警规则并直接发送，面板挂了告警照常能发出。
// 直接发送失败时（例如国内节点访问不了 Telegram）请面板代发；面板也不可达时先存在本地，之后补发。
package alerts

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Relay 请某个面板代发告警到它的渠道。panel 是面板的公钥指纹。
type Relay func(ctx context.Context, panel string, a *nagiv1.Alert, channelIDs []string) error

// Engine 是节点的告警引擎。
type Engine struct {
	log         *slog.Logger
	node        string
	dir         string // 保存告警配置和待补发队列
	certDir     string // Caddy 的证书目录
	metrics     *metrics.Collector
	docker      *docker.Client
	projectName func(id string) string
	relay       Relay

	mu       sync.Mutex
	configs  map[string]*nagiv1.AlertConfig // 面板指纹 → 配置
	states   map[string]*state
	restarts map[string][]restartSample
	memSince time.Time
	queue    []queued
}

type state struct {
	firing       bool
	lastNotified time.Time
	alert        *nagiv1.Alert
}

type restartSample struct {
	at    time.Time
	count float64
}

// queued 是待补发的告警。
type queued struct {
	Alert   json.RawMessage `json:"alert"`
	Channel json.RawMessage `json:"channel"`
	Panel   string          `json:"panel"`
}

// New 创建告警引擎，读取本地保存的配置和待补发队列。
func New(log *slog.Logger, node, dir, certDir string, m *metrics.Collector, dc *docker.Client, projectName func(string) string, relay Relay) *Engine {
	e := &Engine{
		log: log, node: node, dir: dir, certDir: certDir, metrics: m, docker: dc,
		projectName: projectName, relay: relay,
		configs: map[string]*nagiv1.AlertConfig{}, states: map[string]*state{}, restarts: map[string][]restartSample{},
	}
	e.loadConfigs()
	e.loadQueue()
	return e
}

func (e *Engine) configFile() string { return filepath.Join(e.dir, "alerts.json") }
func (e *Engine) queueFile() string  { return filepath.Join(e.dir, "alerts-queue.json") }

// SetConfig 保存某个面板下发的告警配置。
func (e *Engine) SetConfig(panel string, cfg *nagiv1.AlertConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.configs[panel] = cfg
	return e.saveConfigs()
}

func (e *Engine) saveConfigs() error {
	raw := map[string]json.RawMessage{}
	for k, v := range e.configs {
		b, err := protojson.Marshal(v)
		if err != nil {
			return err
		}
		raw[k] = b
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := e.configFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.configFile())
}

func (e *Engine) loadConfigs() {
	data, err := os.ReadFile(e.configFile())
	if err != nil {
		return
	}
	raw := map[string]json.RawMessage{}
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	for k, v := range raw {
		cfg := &nagiv1.AlertConfig{}
		if protojson.Unmarshal(v, cfg) == nil {
			e.configs[k] = cfg
		}
	}
}

// channel 是一个去重后的渠道，以及配置它的面板。
type channel struct {
	ch    *nagiv1.AlertChannel
	panel string
}

// channels 合并所有面板配置的渠道，同一个目的地只发一次。
func (e *Engine) channels() []channel {
	seen := map[string]bool{}
	var out []channel
	panels := make([]string, 0, len(e.configs))
	for p := range e.configs {
		panels = append(panels, p)
	}
	sort.Strings(panels)
	for _, p := range panels {
		for _, c := range e.configs[p].GetChannels() {
			k := alert.ChannelKey(c)
			if !seen[k] {
				seen[k] = true
				out = append(out, channel{ch: c, panel: p})
			}
		}
	}
	return out
}

// rules 取所有面板配置里最严格的阈值。
func (e *Engine) rules() alert.Rules {
	r := alert.EffectiveRules(nil)
	first := true
	for _, cfg := range e.configs {
		x := alert.EffectiveRules(cfg.GetRules())
		if first {
			r, first = x, false
			continue
		}
		r.DiskPercent = min(r.DiskPercent, x.DiskPercent)
		r.MemPercent = min(r.MemPercent, x.MemPercent)
		r.MemFor = min(r.MemFor, x.MemFor)
		r.RestartCount = min(r.RestartCount, x.RestartCount)
		r.RestartWindow = max(r.RestartWindow, x.RestartWindow)
		r.CertDays = max(r.CertDays, x.CertDays)
		r.ErrorPercent = min(r.ErrorPercent, x.ErrorPercent)
	}
	return r
}

// ---- 规则判断 ----

// Run 每 30 秒判断一次规则，并补发队列里的告警，直到 ctx 取消。
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.evaluate(ctx)
		e.flushQueue(ctx)
	}
}

func (e *Engine) evaluate(ctx context.Context) {
	e.mu.Lock()
	rules := e.rules()
	e.mu.Unlock()
	now := time.Now()
	latest := e.metrics.Latest()
	active := map[string]*nagiv1.Alert{}

	if v, ok := latest["node:disk"]; ok && v > rules.DiskPercent {
		active["disk"] = &nagiv1.Alert{Title: fmt.Sprintf("磁盘使用 %.0f%%，超过 %.0f%%", v, rules.DiskPercent)}
	}
	if v, ok := latest["node:mem"]; ok && v > rules.MemPercent {
		if e.memSince.IsZero() {
			e.memSince = now
		}
		if now.Sub(e.memSince) >= rules.MemFor {
			active["mem"] = &nagiv1.Alert{Title: fmt.Sprintf("内存使用 %.0f%%，已持续超过 %d 分钟", v, int(rules.MemFor.Minutes()))}
		}
	} else {
		e.memSince = time.Time{}
	}

	// 容器重启：比较时间窗口内累计重启次数的增量。
	for series, count := range latest {
		name, ok := strings.CutSuffix(series, ":restarts")
		if !ok {
			continue
		}
		hist := append(e.restarts[series], restartSample{at: now, count: count})
		for len(hist) > 0 && now.Sub(hist[0].at) > rules.RestartWindow {
			hist = hist[1:]
		}
		e.restarts[series] = hist
		if n := int(count - hist[0].count); n >= rules.RestartCount {
			project, service := splitContainer(name)
			active["restart:"+name] = &nagiv1.Alert{
				Title:   fmt.Sprintf("服务 %s 在 %d 分钟内重启了 %d 次", service, int(rules.RestartWindow.Minutes()), n),
				Project: e.projectName(project),
			}
		}
	}

	// http 规则的 5xx 比例。
	for route, x := range e.errorRates(ctx, now) {
		if x.requests >= alert.ErrorMinRequests && 100*x.errors/x.requests > rules.ErrorPercent {
			project, name := splitContainer(route)
			active["errors:"+route] = &nagiv1.Alert{
				Title: fmt.Sprintf("规则 %s 最近 %d 分钟 5xx 占 %.0f%%（共 %.0f 个请求）",
					name, int(alert.ErrorWindow.Minutes()), 100*x.errors/x.requests, x.requests),
				Project: e.projectName(project),
			}
		}
	}

	// 健康检查失败。
	if containers, err := e.docker.ManagedContainers(ctx, "nagi.project"); err == nil {
		for _, c := range containers {
			if c.Health == "unhealthy" {
				active["health:"+c.Project+"/"+c.Service] = &nagiv1.Alert{
					Title: fmt.Sprintf("服务 %s 健康检查失败", c.Service), Project: e.projectName(c.Project),
				}
			}
		}
	}

	// 证书快过期。
	for domain, notAfter := range e.certExpiries() {
		if left := time.Until(notAfter); left < time.Duration(rules.CertDays)*24*time.Hour {
			active["cert:"+domain] = &nagiv1.Alert{
				Title: fmt.Sprintf("域名 %s 的证书将在 %d 天内过期", domain, int(left.Hours()/24)),
			}
		}
	}

	e.mu.Lock()
	var send []*nagiv1.Alert
	for key, a := range active {
		a.Key = key
		if x := e.setFiring(key, a); x != nil {
			send = append(send, x)
		}
	}
	for key, st := range e.states {
		// 事件类告警（例如快照失败）由 Fire/Resolve 控制，这里只处理规则类告警。
		if st.firing && !isEventKey(key) && active[key] == nil {
			send = append(send, e.setResolved(key))
		}
	}
	e.mu.Unlock()
	for _, a := range send {
		e.dispatch(ctx, a)
	}
}

type errorRate struct{ requests, errors float64 }

// errorRates 统计每条 http 规则最近一段时间的请求数和 5xx 数（序列存的是每秒速率，每 10 秒一个点）。
func (e *Engine) errorRates(ctx context.Context, now time.Time) map[string]errorRate {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-alert.ErrorWindow), now)
	if err != nil {
		return nil
	}
	out := map[string]errorRate{}
	for series, points := range data {
		route, kind, ok := cutLast(strings.TrimPrefix(series, "r:"), ":")
		if !ok || (kind != "req" && kind != "5xx") {
			continue
		}
		var sum float64
		for _, p := range points {
			sum += p.Value * metrics.Interval.Seconds()
		}
		x := out[route]
		if kind == "req" {
			x.requests = sum
		} else {
			x.errors = sum
		}
		out[route] = x
	}
	return out
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func isEventKey(key string) bool { return strings.HasPrefix(key, "event:") }

func splitContainer(name string) (project, service string) {
	name = strings.TrimPrefix(name, "c:")
	project, service, _ = strings.Cut(name, "/")
	return project, service
}

// certExpiries 读取 Caddy 已经拿到的证书的过期时间。
func (e *Engine) certExpiries() map[string]time.Time {
	out := map[string]time.Time{}
	files, _ := filepath.Glob(filepath.Join(e.certDir, "certificates", "*", "*", "*.crt"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		block, _ := pem.Decode(data)
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		domain := strings.TrimSuffix(filepath.Base(f), ".crt")
		if t, ok := out[domain]; !ok || cert.NotAfter.After(t) {
			out[domain] = cert.NotAfter
		}
	}
	return out
}

// ---- 事件类告警 ----

// Fire 报告一个事件类告警（例如快照失败）。key 相同的告警只在出现时发一次，之后按间隔提醒。
func (e *Engine) Fire(ctx context.Context, key, title, message, projectID string) {
	a := &nagiv1.Alert{Key: "event:" + key, Title: title, Message: message}
	if projectID != "" {
		a.Project = e.projectName(projectID)
	}
	e.mu.Lock()
	send := e.setFiring(a.Key, a)
	e.mu.Unlock()
	if send != nil {
		e.dispatch(ctx, send)
	}
}

// Resolve 报告事件类告警已恢复。
func (e *Engine) Resolve(ctx context.Context, key string) {
	e.mu.Lock()
	var send *nagiv1.Alert
	if st := e.states["event:"+key]; st != nil && st.firing {
		send = e.setResolved("event:" + key)
	}
	e.mu.Unlock()
	if send != nil {
		e.dispatch(ctx, send)
	}
}

// ---- 状态与发送 ----

// setFiring 更新告警状态，需要通知时返回要发送的告警。调用方持有 e.mu。
func (e *Engine) setFiring(key string, a *nagiv1.Alert) *nagiv1.Alert {
	st := e.states[key]
	now := time.Now()
	if st == nil {
		st = &state{}
		e.states[key] = st
	}
	if st.firing && now.Sub(st.lastNotified) < alert.RepeatInterval {
		st.alert = a
		return nil
	}
	st.firing, st.lastNotified, st.alert = true, now, a
	a.Status, a.Node, a.Time = "firing", e.node, timestamppb.New(now)
	return a
}

// setResolved 清除告警状态，返回恢复通知。调用方持有 e.mu。
func (e *Engine) setResolved(key string) *nagiv1.Alert {
	st := e.states[key]
	st.firing = false
	a := &nagiv1.Alert{Key: key, Status: "resolved", Title: st.alert.GetTitle(), Project: st.alert.GetProject(),
		Node: e.node, Time: timestamppb.Now()}
	delete(e.states, key)
	return a
}

// dispatch 发到所有渠道。直接发送失败的，请配置它的面板代发；面板也不可达时放进队列。
// 发送时不持有 e.mu，避免网络慢的时候卡住配置下发。
func (e *Engine) dispatch(ctx context.Context, a *nagiv1.Alert) {
	e.log.Info("告警", "status", a.GetStatus(), "title", a.GetTitle())
	e.mu.Lock()
	chans := e.channels()
	e.mu.Unlock()
	for _, c := range chans {
		if !e.deliver(ctx, a, c.ch, c.panel) {
			e.mu.Lock()
			e.enqueue(a, c)
			e.mu.Unlock()
		}
	}
}

// deliver 先直接发送，失败再请面板代发。
func (e *Engine) deliver(ctx context.Context, a *nagiv1.Alert, ch *nagiv1.AlertChannel, panel string) bool {
	err := alert.Send(ctx, ch, a)
	if err == nil {
		return true
	}
	e.log.Warn("直接发送告警失败，交给面板代发", "channel", ch.GetName(), "err", err)
	return e.relay != nil && e.relay(ctx, panel, a, []string{ch.GetId()}) == nil
}

// enqueue 把发送失败的告警放进待补发队列。调用方持有 e.mu。
func (e *Engine) enqueue(a *nagiv1.Alert, c channel) {
	ab, _ := protojson.Marshal(a)
	cb, _ := protojson.Marshal(c.ch)
	e.queue = append(e.queue, queued{Alert: ab, Channel: cb, Panel: c.panel})
	if len(e.queue) > 200 {
		e.queue = e.queue[len(e.queue)-200:]
	}
	e.saveQueue()
}

func (e *Engine) saveQueue() {
	data, _ := json.Marshal(e.queue)
	_ = os.WriteFile(e.queueFile(), data, 0o600)
}

func (e *Engine) loadQueue() {
	data, err := os.ReadFile(e.queueFile())
	if err == nil {
		_ = json.Unmarshal(data, &e.queue)
	}
}

// flushQueue 补发队列里的告警。
func (e *Engine) flushQueue(ctx context.Context) {
	e.mu.Lock()
	pending := e.queue
	e.queue = nil
	e.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	var rest []queued
	for _, q := range pending {
		a, c := &nagiv1.Alert{}, &nagiv1.AlertChannel{}
		if protojson.Unmarshal(q.Alert, a) != nil || protojson.Unmarshal(q.Channel, c) != nil {
			continue
		}
		if !e.deliver(ctx, a, c, q.Panel) {
			rest = append(rest, q)
		}
	}
	e.mu.Lock()
	e.queue = append(rest, e.queue...)
	e.saveQueue()
	e.mu.Unlock()
}
