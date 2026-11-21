// Package alerts 在节点本地判断告警规则并直接发送，面板挂了告警照常能发出。
// 直接发送失败时（例如国内节点访问不了 Telegram）请面板代发；面板也不可达时先存在本地，之后补发。
package alerts

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Relay 请某个面板代发告警到它的渠道。panel 是面板的公钥指纹。
type Relay func(ctx context.Context, panel string, a *nagiv1.Alert, channelIDs []string) error

type metricSource interface {
	LatestFresh(time.Time) map[string]float64
	Query(context.Context, string, time.Time, time.Time) (map[string][]metrics.Point, error)
}

type containerSource interface {
	ManagedInstances(context.Context, string) ([]docker.Instance, error)
}

// Engine 是节点的告警引擎。
type Engine struct {
	log         *slog.Logger
	logs        *logstore.Store
	node        string // 主机名；面板下发了节点名称时用面板的（见 nodeName）
	dir         string // 保存告警配置和待补发队列
	certDir     string // Caddy 的证书目录
	metrics     metricSource
	docker      containerSource
	projectName func(id string) string
	relay       Relay
	// Report 告诉面板发出了一条告警，记入活动记录。发送结果不影响告警本身。
	Report func(context.Context, *nagiv1.Alert)
	// Domains 返回现在还在用的域名（项目映射和面板域名），只检查这些域名的证书。为 nil 时检查全部。
	Domains func() map[string]bool
	// ContainerRules 返回容器生效的告警规则（面板随容器配置下发）。旧版本的面板不下发，ok 为 false，
	// 这时按告警配置里的规则判断。RouteRules 同样返回 http 规则所属容器的规则，用于判断 5xx 比例。
	ContainerRules func(projectID, container string) (*nagiv1.AlertRules, bool)
	RouteRules     func(projectID, route string) (*nagiv1.AlertRules, bool)

	mu         sync.Mutex
	configs    map[string]*nagiv1.AlertConfig // 面板指纹 → 配置
	states     map[string]*state
	restarts   map[string][]restartSample
	memSince   time.Time
	load       loadState
	queue      []queued
	source     string
	revision   uint64
	history    []*nagiv1.Alert
	stateErr   error                // fail closed: never publish an uncommitted source version
	suppressed map[string]time.Time // occurrence -> bounded primary-authorized mute
}

type state struct {
	firing        bool
	lastNotified  time.Time
	lastDelivered time.Time // only accepted deliveries suppress repeats after restart
	alert         *nagiv1.Alert
	occurrence    string
	since         time.Time
	confirmed     bool
}

type restartSample struct {
	at    time.Time
	count float64
}

// New 创建告警引擎，读取本地保存的配置和待补发队列。
func New(log *slog.Logger, node, dir, certDir string, m metricSource, dc *docker.Client, projectName func(string) string, relay Relay, logs *logstore.Store) *Engine {
	e := &Engine{
		log: log, logs: logs, node: node, dir: dir, certDir: certDir, metrics: m, docker: dc,
		projectName: projectName, relay: relay,
		configs: map[string]*nagiv1.AlertConfig{}, states: map[string]*state{}, restarts: map[string][]restartSample{},
	}
	e.loadConfigs()
	e.loadStates()
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

// Retain 删除不在列表里的面板的告警配置（面板已被移出信任列表）。
func (e *Engine) Retain(panels []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	for p := range e.configs {
		if !slices.Contains(panels, p) {
			delete(e.configs, p)
			changed = true
		}
	}
	if changed {
		if err := e.saveConfigs(); err != nil {
			e.log.Warn("保存告警配置失败", "err", err)
		}
	}
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

// nodeName 是告警里的节点名称：面板上的名称（用户可能改过），和界面上一致；面板没下发时用主机名。
// 移交过程中两个面板的名称可能不同，按面板指纹排序取第一个，结果固定。调用方持有 e.mu。
func (e *Engine) nodeName() string {
	panels := make([]string, 0, len(e.configs))
	for p := range e.configs {
		panels = append(panels, p)
	}
	sort.Strings(panels)
	for _, p := range panels {
		if n := e.configs[p].GetNodeName(); n != "" {
			return n
		}
	}
	return e.node
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

// nodeEnabled 判断节点的规则开着没有：任何一个面板的配置里开着就判断（和阈值一样取最严格的）。调用方持有 e.mu。
func (e *Engine) nodeEnabled(rule string) bool {
	if len(e.configs) == 0 {
		return true
	}
	for _, cfg := range e.configs {
		if alert.Enabled(cfg.GetRules(), rule) {
			return true
		}
	}
	return false
}

// NodeEnabled 判断节点的规则开着没有，给事件类告警（证书申请失败）用。
func (e *Engine) NodeEnabled(rule string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodeEnabled(rule)
}

// judge 是判断一个容器用的规则：阈值和开关。
type judge struct {
	alert.Rules
	on func(rule string) bool
}

// forRules 按面板下发的容器规则判断；没有下发（旧版本的面板）时用告警配置里的。
func forRules(r *nagiv1.AlertRules, ok bool, fallback judge) judge {
	if !ok {
		return fallback
	}
	return judge{alert.EffectiveRules(r), func(rule string) bool { return alert.Enabled(r, rule) }}
}

func (e *Engine) forContainer(projectID, container string, fallback judge) judge {
	if e.ContainerRules == nil {
		return fallback
	}
	r, ok := e.ContainerRules(projectID, container)
	return forRules(r, ok, fallback)
}

// ContainerEnabled 判断容器的规则开着没有，给事件类告警（快照失败、备份同步失败）用。
func (e *Engine) ContainerEnabled(projectID, container, rule string) bool {
	e.mu.Lock()
	fallback := e.nodeEnabled(rule)
	e.mu.Unlock()
	return e.forContainer(projectID, container, judge{on: func(string) bool { return fallback }}).on(rule)
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
	enabled := map[string]bool{}
	for _, k := range append(append([]string{}, alert.NodeRules...), alert.ContainerRules...) {
		enabled[k] = e.nodeEnabled(k)
	}
	e.mu.Unlock()
	node := judge{rules, func(rule string) bool { return enabled[rule] }}
	now := time.Now()
	latest := e.metrics.LatestFresh(now)
	active := map[string]*nagiv1.Alert{}
	// Only an explicitly evaluated, healthy item may resolve an existing alert.
	// An absent series, disabled rule or failed query is not a healthy result.
	healthy := map[string]bool{}

	if v, ok := latest["node:disk"]; ok && enabled[alert.RuleDisk] {
		if v > rules.DiskPercent {
			active["disk"] = &nagiv1.Alert{Title: fmt.Sprintf("磁盘使用 %.0f%%，超过 %.0f%%", v, rules.DiskPercent)}
		} else {
			healthy["disk"] = true
		}
	}
	for series, v := range latest {
		if mount, ok := strings.CutPrefix(series, "node:disk@"); ok && enabled[alert.RuleDisk] {
			if v > rules.DiskPercent {
				active["disk:"+mount] = &nagiv1.Alert{Title: fmt.Sprintf("磁盘 %s（项目目录所在）使用 %.0f%%，超过 %.0f%%", mount, v, rules.DiskPercent)}
			} else {
				healthy["disk:"+mount] = true
			}
		}
	}
	if v, ok := latest["node:mem"]; ok && v > rules.MemPercent && enabled[alert.RuleMem] {
		if e.memSince.IsZero() {
			e.memSince = now
		}
		if now.Sub(e.memSince) >= rules.MemFor {
			active["mem"] = &nagiv1.Alert{Title: fmt.Sprintf("内存使用 %.0f%%，已持续超过 %d 分钟", v, int(rules.MemFor.Minutes()))}
		}
	} else {
		e.memSince = time.Time{}
		if v, ok := latest["node:mem"]; ok && v <= rules.MemPercent && enabled[alert.RuleMem] {
			healthy["mem"] = true
		}
	}

	// 容器重启：比较时间窗口内累计重启次数的增量。
	for series := range e.restarts {
		if _, ok := latest[series]; !ok {
			delete(e.restarts, series)
		}
	}
	for series, count := range latest {
		name, ok := strings.CutSuffix(series, ":restarts")
		if !ok {
			continue
		}
		project, service := splitContainer(name)
		container, _, _ := strings.Cut(service, "/")
		j := e.forContainer(project, container, node)
		hist := e.restarts[series]
		if len(hist) > 0 && (count < hist[len(hist)-1].count || now.Sub(hist[len(hist)-1].at) > time.Minute) {
			hist = nil
		}
		hist = append(hist, restartSample{at: now, count: count})
		for len(hist) > 1 && now.Sub(hist[1].at) >= j.RestartWindow {
			hist = hist[1:]
		}
		e.restarts[series] = hist
		baseline := hist[0]
		if now.Sub(baseline.at) > j.RestartWindow && len(hist) > 1 {
			baseline = hist[1]
		}
		if n := int(count - baseline.count); n >= j.RestartCount && j.on(alert.RuleRestart) {
			active["restart:"+name] = &nagiv1.Alert{
				Title:   fmt.Sprintf("%s 在 %d 分钟内重启了 %d 次", describeService(service), int(j.RestartWindow.Minutes()), n),
				Project: e.projectName(project),
			}
		} else if j.on(alert.RuleRestart) && now.Sub(hist[0].at) >= j.RestartWindow && count >= hist[0].count {
			// After a restart or a sampling gap, rebuild a full window before recovery.
			healthy["restart:"+name] = true
		}
	}

	// http 规则的 5xx 比例。
	for route, x := range e.errorRates(ctx, now) {
		project, name := splitContainer(route)
		j := node
		if e.RouteRules != nil {
			r, ok := e.RouteRules(project, name)
			j = forRules(r, ok, node)
		}
		if x.requests >= alert.ErrorMinRequests && 100*x.errors/x.requests > j.ErrorPercent && j.on(alert.RuleErrors) {
			active["errors:"+route] = &nagiv1.Alert{
				Title: fmt.Sprintf("规则 %s 最近 %d 分钟 5xx 占 %.0f%%（共 %.0f 个请求）",
					name, int(alert.ErrorWindow.Minutes()), 100*x.errors/x.requests, x.requests),
				Project: e.projectName(project),
			}
		} else if x.requests >= alert.ErrorMinRequests && j.on(alert.RuleErrors) {
			healthy["errors:"+route] = true
		}
	}

	// CPU 和流量。
	e.evaluateLoad(ctx, now, latest, node, enabled, active, healthy)

	// 健康检查失败。
	if containers, err := e.docker.ManagedInstances(ctx, "nagi.project"); err == nil {
		for _, c := range containers {
			key := "health:" + c.Project + "/" + c.Container + "/" + c.Service
			if !e.forContainer(c.Project, c.Container, node).on(alert.RuleHealth) {
				continue
			}
			if c.Health == "unhealthy" {
				what := describeService(c.Container + "/" + c.Service)
				active["health:"+c.Project+"/"+c.Container+"/"+c.Service] = &nagiv1.Alert{
					Title: what + " 健康检查失败", Project: e.projectName(c.Project),
				}
			} else if c.State == "running" && c.Health == "healthy" {
				healthy[key] = true
			}
		}
	}

	// 证书快过期。
	for domain, notAfter := range e.CertExpiries() {
		if left := time.Until(notAfter); left < time.Duration(rules.CertDays)*24*time.Hour && enabled[alert.RuleCert] {
			active["cert:"+domain] = &nagiv1.Alert{
				Title: fmt.Sprintf("域名 %s 的证书将在 %d 天内过期", domain, int(left.Hours()/24)),
			}
		} else if enabled[alert.RuleCert] {
			healthy["cert:"+domain] = true
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
		if st.firing && !isEventKey(key) && active[key] == nil && healthy[key] {
			send = append(send, e.setResolved(key))
		} else if !isEventKey(key) {
			st.confirmed = active[key] != nil
		}
	}
	e.saveStates()
	e.mu.Unlock()
	for _, a := range send {
		e.dispatch(ctx, a)
	}
}

type errorRate struct{ requests, errors float64 }

// errorRates 统计每条 http 规则最近一段时间的请求数和 5xx 数（序列存的是每秒速率，每 10 秒一个点）。
func (e *Engine) errorRates(ctx context.Context, now time.Time) map[string]errorRate {
	return e.windowRates(ctx, now, "5xx")
}

// windowRates requires matching, fresh request and status samples throughout
// the window. A missing error counter is not a zero error counter.
func (e *Engine) windowRates(ctx context.Context, now time.Time, kind string) map[string]errorRate {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-alert.ErrorWindow), now)
	if err != nil {
		return nil
	}
	out := map[string]errorRate{}
	for series, points := range data {
		route, ok := strings.CutSuffix(strings.TrimPrefix(series, "r:"), ":req")
		if !ok || len(points) == 0 {
			continue
		}
		errors := data["r:"+route+":"+kind]
		if len(errors) != len(points) {
			continue
		}
		previous := now.Add(-alert.ErrorWindow)
		x := errorRate{}
		valid := true
		for i, p := range points {
			e := errors[i]
			if !e.Time.Equal(p.Time) || p.Time.Before(previous) || p.Time.After(now) || p.Time.Sub(previous) > 3*metrics.Interval ||
				math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || math.IsNaN(e.Value) || math.IsInf(e.Value, 0) || p.Value < 0 || e.Value < 0 || e.Value > p.Value {
				valid = false
				break
			}
			x.requests += p.Value * metrics.Interval.Seconds()
			x.errors += e.Value * metrics.Interval.Seconds()
			previous = p.Time
		}
		if valid && now.Sub(previous) <= 3*metrics.Interval {
			out[route] = x
		}
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

// describeService 把"容器/服务"写成给人看的样子：单个容器（服务名就是容器名）写"容器 X"，
// 容器组里的写"容器组 X 里的容器 Y"（compose 里的一个服务就是组里的一个容器）。
func describeService(containerService string) string {
	container, service, _ := strings.Cut(containerService, "/")
	if service == "" || service == container {
		return "容器 " + container
	}
	return "容器组 " + container + " 里的容器 " + service
}

func splitContainer(name string) (project, service string) {
	name = strings.TrimPrefix(name, "c:")
	project, service, _ = strings.Cut(name, "/")
	return project, service
}

// CertExpiries 读取 Caddy 已经拿到的证书的过期时间。跳过两种：
//   - Caddy 本地 CA 签的（.localhost 等内部域名，certificates/local/ 下），有效期本来就只有几小时，Caddy 自动续；
//   - 已经不用的域名（解绑以后旧证书还留在存储里，不会再续）。
func (e *Engine) CertExpiries() map[string]time.Time {
	out := map[string]time.Time{}
	var inUse map[string]bool
	if e.Domains != nil {
		inUse = e.Domains()
	}
	files, _ := filepath.Glob(filepath.Join(e.certDir, "certificates", "*", "*", "*.crt"))
	for _, f := range files {
		if filepath.Base(filepath.Dir(filepath.Dir(f))) == "local" {
			continue
		}
		if inUse != nil && !inUse[strings.TrimSuffix(filepath.Base(f), ".crt")] {
			continue
		}
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
	e.saveStates()
	e.mu.Unlock()
	if send != nil {
		e.dispatch(ctx, send)
	}
}

// Notify 发一条一次性的通知（例如"已顶替项目"），不跟踪状态，也没有"已恢复"。
func (e *Engine) Notify(ctx context.Context, title, message, projectID string) {
	e.mu.Lock()
	node := e.nodeName()
	e.mu.Unlock()
	a := &nagiv1.Alert{Key: "notice", Status: "info", Title: title, Message: message, Node: node, Time: timestamppb.Now()}
	if projectID != "" {
		a.Project = e.projectName(projectID)
	}
	e.dispatch(ctx, a)
}

// NotifyKey 和 Notify 一样发一条一次性的通知，带上 key（面板据此归类，例如出站限制的 egress:<容器>:<目标>）。
func (e *Engine) NotifyKey(ctx context.Context, key, title, message, projectID string) {
	e.mu.Lock()
	node := e.nodeName()
	e.mu.Unlock()
	a := &nagiv1.Alert{Key: key, Status: "info", Title: title, Message: message, Node: node, Time: timestamppb.Now()}
	if projectID != "" {
		a.Project = e.projectName(projectID)
	}
	e.dispatch(ctx, a)
}

// Resolve 报告事件类告警已恢复。
func (e *Engine) Resolve(ctx context.Context, key string) {
	e.mu.Lock()
	var send *nagiv1.Alert
	if st := e.states["event:"+key]; st != nil && st.firing {
		send = e.setResolved("event:" + key)
	}
	e.saveStates()
	e.mu.Unlock()
	if send != nil {
		e.dispatch(ctx, send)
	}
}

// MarkUnavailableUnless invalidates removed objects without inventing recovery.
func (e *Engine) MarkUnavailableUnless(prefix string, keep func(rest string) bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, st := range e.states {
		if rest, ok := strings.CutPrefix(k, "event:"+prefix); ok && st.firing && !keep(rest) {
			st.confirmed = false
		}
	}
	e.saveStates()
}

// ---- 状态与发送 ----

// setFiring 更新告警状态，需要通知时返回要发送的告警。调用方持有 e.mu。
func (e *Engine) setFiring(key string, a *nagiv1.Alert) *nagiv1.Alert {
	st := e.states[key]
	now := time.Now()
	if st == nil {
		if len(e.states) >= 200 {
			return nil
		}
		st = &state{occurrence: rand.Text(), since: now}
		e.states[key] = st
	}
	st.confirmed = true
	a.Status, a.Node, a.Time = "firing", e.nodeName(), timestamppb.New(now)
	a.Occurrence, a.Source, a.Since, a.Validity = st.occurrence, e.source, timestamppb.New(st.since), "valid"
	pending := false
	for _, q := range e.queue {
		pending = pending || q.Source == e.source && q.Occurrence == st.occurrence
	}
	if st.firing && now.Sub(st.lastNotified) < alert.RepeatInterval && (!st.lastDelivered.IsZero() || pending || len(e.channels()) == 0) {
		st.alert = a
		return nil
	}
	st.firing, st.lastNotified, st.alert = true, now, a
	return a
}

// setResolved 清除告警状态，返回恢复通知。调用方持有 e.mu。
func (e *Engine) setResolved(key string) *nagiv1.Alert {
	return e.end(key, "recovered")
}

func (e *Engine) end(key, reason string) *nagiv1.Alert {
	st := e.states[key]
	st.firing = false
	status := "ended"
	if reason == "recovered" {
		status = "resolved"
	}
	a := &nagiv1.Alert{Key: key, Status: status, Title: st.alert.GetTitle(), Project: st.alert.GetProject(),
		Node: e.nodeName(), Time: timestamppb.Now(), Occurrence: st.occurrence, Source: e.source,
		Since: timestamppb.New(st.since), EndReason: reason, Validity: "unknown"}
	if reason == "recovered" {
		a.Validity = "valid"
	}
	e.history = append(e.history, a)
	if len(e.history) > 200 {
		e.history = e.history[len(e.history)-200:]
	}
	delete(e.states, key)
	return a
}

// dispatch 发到所有渠道。直接发送失败的，请配置它的面板代发；面板也不可达时放进队列。
// 发送时不持有 e.mu，避免网络慢的时候卡住配置下发。
func (e *Engine) dispatch(ctx context.Context, a *nagiv1.Alert) {
	e.mu.Lock()
	if a.GetStatus() != "info" {
		a = e.notificationAlert(queued{Source: a.GetSource(), Occurrence: a.GetOccurrence(), Key: a.GetKey(), Status: a.GetStatus()})
	}
	if a == nil {
		e.mu.Unlock()
		return
	}
	a = proto.CloneOf(a)
	chans := e.channels()
	e.mu.Unlock()
	e.log.Info("告警", "status", a.GetStatus(), "title", a.GetTitle())
	if e.Report != nil {
		go e.Report(context.WithoutCancel(ctx), a)
	}
	accepted := len(chans) > 0
	for _, c := range chans {
		if !e.deliver(ctx, a, c.ch, c.panel, false) {
			accepted = false
			e.mu.Lock()
			e.enqueue(a, c)
			e.mu.Unlock()
		}
	}
	if accepted && a.GetStatus() == "firing" {
		e.mu.Lock()
		if st := e.states[a.GetKey()]; st != nil && st.occurrence == a.GetOccurrence() && st.confirmed {
			st.lastDelivered = st.lastNotified
			e.saveStates()
		}
		e.mu.Unlock()
	}
}

// Revalidate between network attempts; config changes cannot redirect a delayed
// relay to a replacement destination with the same channel ID.
// A legacy (v0.36) queue entry has no occurrence to revalidate; only its channel.
func (e *Engine) currentDelivery(a *nagiv1.Alert, ch *nagiv1.AlertChannel, panel string, legacy bool) (*nagiv1.Alert, *nagiv1.AlertChannel) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, current := range e.configs[panel].GetChannels() {
		if current.GetId() != ch.GetId() || destination(current) != destination(ch) {
			continue
		}
		if a.GetStatus() != "info" && !legacy {
			a = e.notificationAlert(queued{Source: a.GetSource(), Occurrence: a.GetOccurrence(), Key: a.GetKey(), Status: a.GetStatus()})
		}
		if a != nil {
			return proto.CloneOf(a), proto.CloneOf(current)
		}
	}
	return nil, nil
}

// deliver 先直接发送，失败再请面板代发。
func (e *Engine) deliver(ctx context.Context, a *nagiv1.Alert, ch *nagiv1.AlertChannel, panel string, legacy bool) bool {
	a, ch = e.currentDelivery(a, ch, panel, legacy)
	if a == nil || ch == nil {
		return false
	}
	err := alert.Send(ctx, ch, a)
	if err == nil {
		return true
	}
	e.log.Warn("直接发送告警失败，交给面板代发", "channel", ch.GetName(), "err", err)
	a, ch = e.currentDelivery(a, ch, panel, legacy)
	if a == nil || ch == nil {
		return false
	}
	return e.relay != nil && e.relay(ctx, panel, a, []string{ch.GetId()}) == nil
}
