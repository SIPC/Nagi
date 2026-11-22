package alerts

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 规则的判断（见 docs/monitoring.md#告警）：每 30 秒收集一次各告警键的观测，交给状态机（machine.go）。
// 这里只报告单项事实，不把几个信号合起来推断原因（攻击、挖矿等交给模型报告）。

// baselineRefresh：过去 24 小时的平均请求量隔这么久重算一次。
const baselineRefresh = 5 * time.Minute

// loadState 是需要跨判断保留的数据（不保存）。
type loadState struct {
	restarts map[string][]restartSample
	// baseline：http 映射 → 过去 24 小时的请求量基线；l4Base：tcp/udp 映射 → 新连接基线（见 sources.go）。
	baseline   map[string]routeBaseline
	l4Base     map[string]routeBaseline
	baselineAt time.Time
}

type restartSample struct {
	at    time.Time
	count float64
}

// threshold 按规则判断一个值：超过触发阈值是 bad，回到恢复阈值的好的一侧是 good（alert.Def.Recovered）。
// lower 的规则（证书）反过来。
func threshold(o *obs, v float64) *obs {
	r := o.set.get(o.rule)
	d, _ := alert.DefOf(o.rule)
	o.value, o.hasValue = v, true
	o.threshold, o.recover = r.GetThreshold(), r.GetRecoverThreshold()
	if d.Lower {
		o.bad = v < o.threshold
	} else {
		o.bad = v > o.threshold
	}
	o.good = d.Recovered(v, o.recover)
	o.disabled = !alert.On(r)
	return o
}

// observe 收集这一轮的观测：键 → 观测；没有数据的键不在里面。
func (e *Engine) observe(ctx context.Context, now time.Time) map[string]*obs {
	e.mu.Lock()
	node := e.nodeRules()
	e.mu.Unlock()
	latest := e.metrics.LatestFresh(now)
	out := map[string]*obs{}

	for series, v := range latest {
		switch {
		case series == "node:disk":
			out["disk"] = threshold(&obs{rule: alert.RuleDisk, set: node}, v)
		case strings.HasPrefix(series, "node:disk@"):
			mount := strings.TrimPrefix(series, "node:disk@")
			out["disk:"+mount] = threshold(&obs{rule: alert.RuleDisk, subject: mount, set: node}, v)
		case series == "node:mem":
			out["mem"] = threshold(&obs{rule: alert.RuleMem, set: node}, v)
		case series == "node:cpu":
			out["cpu"] = threshold(&obs{rule: alert.RuleCPU, set: node}, v)
		}
	}

	observeHost(latest, node, out)
	e.mu.Lock()
	e.observeRestarts(now, latest, out)
	e.observeContainerCPU(latest, out)
	e.observeCounts(now, out)
	e.mu.Unlock()
	e.observeRoutes(ctx, now, out)
	e.observeL4(ctx, now, out)
	e.observeFacts(ctx, now, out)

	if containers, err := e.docker.ManagedInstances(ctx, "nagi.project"); err == nil {
		e.mu.Lock()
		for _, c := range containers {
			key := "health:" + c.Project + "/" + c.Container + "/" + c.Service
			set := e.forContainer(c.Project, c.Container)
			o := &obs{rule: alert.RuleHealth, subject: describeService(c.Container + "/" + c.Service), project: c.Project, set: set,
				disabled: !set.on(alert.RuleHealth)}
			switch {
			case c.Health == "unhealthy":
				o.bad = true
			case c.State == "running" && c.Health == "healthy":
				o.good = true
			default:
				continue // starting、没有健康检查、没在运行：不是健康的证据
			}
			out[key] = o
		}
		e.mu.Unlock()
	}

	for domain, notAfter := range e.CertExpiries() {
		days := math.Floor(notAfter.Sub(now).Hours() / 24)
		out["cert:"+domain] = threshold(&obs{rule: alert.RuleCert, subject: domain, set: node}, days)
	}
	return out
}

// observeRestarts 比较时间窗口内累计重启次数的增量。调用方持有 e.mu。
func (e *Engine) observeRestarts(now time.Time, latest map[string]float64, out map[string]*obs) {
	if e.load.restarts == nil {
		e.load.restarts = map[string][]restartSample{}
	}
	for series := range e.load.restarts {
		if _, ok := latest[series]; !ok {
			delete(e.load.restarts, series)
		}
	}
	for series, count := range latest {
		name, ok := strings.CutSuffix(series, ":restarts")
		if !ok {
			continue
		}
		project, service := splitContainer(name)
		container, _, _ := strings.Cut(service, "/")
		set := e.forContainer(project, container)
		r := set.get(alert.RuleRestart)
		window := time.Duration(max(1, r.GetWindowMinutes())) * time.Minute
		hist := e.load.restarts[series]
		if len(hist) > 0 && (count < hist[len(hist)-1].count || now.Sub(hist[len(hist)-1].at) > time.Minute) {
			hist = nil // 计数回退（容器重建）或断档：重新建立窗口
		}
		hist = append(hist, restartSample{at: now, count: count})
		for len(hist) > 1 && now.Sub(hist[1].at) >= window {
			hist = hist[1:]
		}
		e.load.restarts[series] = hist
		n := count - hist[0].count
		o := &obs{rule: alert.RuleRestart, subject: describeService(service), project: project, set: set}
		threshold(o, n)
		o.bad = n >= o.threshold
		// 恢复要覆盖一整个窗口没有重启（断档或重建后要重新攒够窗口）。
		o.good = n == 0 && now.Sub(hist[0].at) >= window-time.Second
		if !o.bad && !o.good && !o.disabled {
			continue
		}
		out["restart:"+name] = o
	}
}

// observeContainerCPU：容器组里各服务加起来，和 l:<项目>/<容器>:cpu（各服务上限之和，没有上限时是整机）比。调用方持有 e.mu。
func (e *Engine) observeContainerCPU(latest map[string]float64, out map[string]*obs) {
	used := map[string]float64{}
	for series, v := range latest {
		name, ok := strings.CutSuffix(series, ":cpu")
		if !ok || !strings.HasPrefix(name, "c:") {
			continue
		}
		project, service := splitContainer(strings.TrimPrefix(name, "c:"))
		container, _, _ := strings.Cut(service, "/")
		used[project+"/"+container] += v
	}
	for key, v := range used {
		limit := latest["l:"+key+":cpu"]
		if limit <= 0 {
			continue
		}
		project, container, _ := strings.Cut(key, "/")
		o := &obs{rule: alert.RuleContainerCPU, subject: describeService(container), project: project, set: e.forContainer(project, container)}
		out["container_cpu:"+key] = threshold(o, 100*v/limit)
	}
}

// observeRoutes 按 http 映射判断 5xx、4xx 比例和请求量突增。
func (e *Engine) observeRoutes(ctx context.Context, now time.Time, out map[string]*obs) {
	e.mu.Lock()
	refresh := now.Sub(e.load.baselineAt) >= baselineRefresh
	e.mu.Unlock()
	var base, l4 map[string]routeBaseline
	if refresh {
		base, l4 = e.computeBaselines(ctx, now)
	}
	// 统计窗口按规则可以不同，同一个窗口只查一次。
	cache := map[time.Duration]map[string]routeWindow{}
	window := func(d time.Duration) map[string]routeWindow {
		if w, ok := cache[d]; ok {
			return w
		}
		w := e.routeWindows(ctx, now, d)
		cache[d] = w
		return w
	}
	e.mu.Lock()
	if refresh && base != nil {
		e.load.baseline, e.load.l4Base, e.load.baselineAt = base, l4, now
	}
	routes := map[string]bool{}
	for route := range window(alert.ErrorWindowDefault) {
		routes[route] = true
	}
	sets := map[string]ruleSet{}
	for route := range routes {
		project, name := splitContainer(route)
		sets[route] = e.forRoute(project, name)
	}
	baseline := e.load.baseline
	e.mu.Unlock()

	for route, set := range sets {
		project, name := splitContainer(route)
		for _, rule := range []string{alert.RuleErrors, alert.RuleClientErrors, alert.RuleTraffic} {
			r := set.get(rule)
			span := time.Duration(max(1, r.GetWindowMinutes())) * time.Minute
			x, ok := window(span)[route]
			if !ok {
				continue
			}
			o := &obs{rule: rule, subject: name, project: project, set: set}
			switch rule {
			case alert.RuleErrors, alert.RuleClientErrors:
				bad := x.server
				if rule == alert.RuleClientErrors {
					bad = x.client
				}
				if bad < 0 { // 没有这种状态码的计数，不能当作 0
					continue
				}
				if x.requests < r.GetMinRequests() || x.requests == 0 {
					o.lowData, o.disabled = true, !alert.On(r)
					out[rule+":"+route] = o
					continue
				}
				threshold(o, 100*bad/x.requests)
				o.message = requestsNote(set.lang, x.requests, span)
			case alert.RuleTraffic:
				rate := x.requests / span.Seconds()
				b := baseline[route].rate
				if b <= 0 { // 没有历史：不判断
					continue
				}
				threshold(o, rate/b)
				// 每秒请求数没到下限时不算突增；回到下限以下也算恢复。
				if rate <= r.GetMinRequests() {
					o.bad, o.good = false, true
				}
				o.message = rateNote(set.lang, rate, b)
			}
			out[rule+":"+route] = o
		}
	}
}

func requestsNote(lang string, n float64, span time.Duration) string {
	if lang == "en" {
		return "Requests in the last " + alert.Duration(span, true) + ": " + alert.Num(n)
	}
	return "最近 " + alert.Duration(span, false) + "共 " + alert.Num(n) + " 个请求"
}

func rateNote(lang string, rate, base float64) string {
	if lang == "en" {
		return "Now " + alert.Num(rate) + " req/s, 24 h average " + alert.Num(base) + " req/s"
	}
	return "现在每秒 " + alert.Num(rate) + " 个请求，过去 24 小时平均每秒 " + alert.Num(base) + " 个"
}

// routeWindow 是一条映射在一个窗口里的请求数、4xx 数和 5xx 数；没有对应的计数时为 -1。
type routeWindow struct{ requests, client, server float64 }

func (e *Engine) routeWindows(ctx context.Context, now time.Time, span time.Duration) map[string]routeWindow {
	req := e.windowRates(ctx, now, span, "req")
	c4 := e.windowRates(ctx, now, span, "4xx")
	c5 := e.windowRates(ctx, now, span, "5xx")
	out := map[string]routeWindow{}
	for route, x := range req {
		w := routeWindow{requests: x.requests, client: -1, server: -1}
		if y, ok := c4[route]; ok {
			w.client = y.errors
		}
		if y, ok := c5[route]; ok {
			w.server = y.errors
		}
		out[route] = w
	}
	return out
}

type errorRate struct{ requests, errors float64 }

// windowRates requires matching, fresh request and status samples throughout
// the window. A missing error counter is not a zero error counter.
func (e *Engine) windowRates(ctx context.Context, now time.Time, span time.Duration, kind string) map[string]errorRate {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-span), now)
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
		previous := now.Add(-span)
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

// evaluate 判断一轮：收集观测，推进每个告警键的状态机，发出通知。
func (e *Engine) evaluate(ctx context.Context) {
	now := e.now()
	seen := e.observe(ctx, now)
	e.mu.Lock()
	var send []*nagiv1.Alert
	keys := map[string]bool{}
	for k := range seen {
		keys[k] = true
	}
	for k := range e.states {
		if !isEventKey(k) {
			keys[k] = true
		}
	}
	for k := range e.tracks {
		keys[k] = true
	}
	for key := range keys {
		o := seen[key]
		if o == nil {
			// 不再观测到的键：规则关掉时结束；否则缺数据，不动。
			if rule, set := e.rulesFor(key); rule != "" && !set.on(rule) {
				o = &obs{rule: rule, disabled: true, set: set}
			} else if strings.HasPrefix(key, "cert:") && e.Domains != nil && !e.Domains()[strings.TrimPrefix(key, "cert:")] {
				if st := e.states[key]; st != nil && st.firing {
					e.end(key, "object_removed")
				}
				delete(e.tracks, key)
				continue
			}
		}
		// 关掉规则、对象不在了是有意的操作，只记录不通知；数据不足要告诉人（这不是恢复）。抖动中不逐条通知。
		if a := e.step(key, o, now); a != nil && !(a.GetStatus() != "firing" && e.flapActive(key)) &&
			(a.GetStatus() != "ended" || a.GetEndReason() == "insufficient_data") {
			send = append(send, a)
		}
		if e.states[key] == nil && e.tracks[key] != nil && e.tracks[key].badSince.IsZero() && seen[key] == nil {
			delete(e.tracks, key)
		}
	}
	send = append(send, e.settleFlaps(now)...)
	send = append(send, e.notices...)
	e.notices = nil
	if len(send) > 0 {
		e.announceRev = e.revision + 1 // saveStates writes this version
	}
	e.saveStates()
	e.mu.Unlock()
	for _, a := range send {
		e.dispatch(ctx, a)
	}
}
