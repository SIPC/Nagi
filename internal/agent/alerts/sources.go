package alerts

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// v0.37.1 新增的规则（见 docs/monitoring.md#告警）：每条只看一个信号，交给同一个状态机。
//   - 指标类（inode、conntrack、网卡、L4）读监控序列；
//   - 状态类（mesh 成员、见证人、日志库、节点证书、快照、副本）由 agent 通过 Probes 报告当前事实；
//   - 次数类（日志库写入失败、内核磁盘错误、内核 OOM、容器 OOM）由 Count 记下每次发生，按窗口数。

// Fact 是 agent 报告的一个告警键的当前事实。
type Fact struct {
	Key     string // 告警键，例如 mesh_peer:<公钥前缀>
	Rule    string
	Subject string
	Project string // 项目 ID（容器的规则）
	// Container 用来取容器的规则（项目 ID 不为空时）。
	Container string
	// Value、HasValue：有数值时按规则的阈值判断。
	Value    float64
	HasValue bool
	// Bad、Good：没有数值的规则（mesh 成员断开、见证人、端口监听）直接给出判断；都为 false 是没有证据。
	// Judged 为 true 时有数值也按 Bad、Good 判断（数值只用来显示，例如节点证书剩余天数）。
	Bad, Good, Judged bool
	Message           string
}

// Probes 返回这一轮 agent 能报告的事实（节点规则和容器规则都可以）。为 nil 时没有这类规则。
type Probes func(ctx context.Context, now time.Time) []Fact

// counted 是次数类规则在一个告警键上的发生时间。
type counted struct {
	rule, subject, project, container string
	at                                []time.Time
	// fresh：上一轮判断以后又发生过（这一轮不算恢复）。
	fresh bool
}

// Count 记下一次发生（日志库写入失败、内核磁盘错误、OOM 等）。key 是告警键。可以并发调用。
func (e *Engine) Count(key, rule, subject, project, container string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.counts == nil {
		e.counts = map[string]*counted{}
	}
	c := e.counts[key]
	if c == nil {
		if len(e.counts) >= 500 {
			return
		}
		c = &counted{rule: rule, subject: subject, project: project, container: container}
		e.counts[key] = c
	}
	c.at, c.fresh = append(c.at, at), true
	if len(c.at) > 1000 {
		c.at = c.at[len(c.at)-1000:]
	}
}

// observeCounts：窗口内发生了 threshold 次以上是 bad；上一轮以后没有新的是 good，状态机再要求连续
// recover_seconds 都是 good 才恢复（「连续 recover_seconds 没有新的」）。调用方持有 e.mu。
func (e *Engine) observeCounts(now time.Time, out map[string]*obs) {
	for key, c := range e.counts {
		set := e.nodeRules()
		if c.project != "" {
			set = e.forContainer(c.project, c.container)
		}
		window := time.Duration(max(1, set.get(c.rule).GetWindowMinutes())) * time.Minute
		kept := c.at[:0]
		for _, t := range c.at {
			if now.Sub(t) < window {
				kept = append(kept, t)
			}
		}
		c.at = kept
		n := len(c.at)
		o := &obs{rule: c.rule, subject: c.subject, project: c.project, set: set}
		threshold(o, float64(n))
		o.bad = float64(n) >= o.threshold
		o.good = !c.fresh
		c.fresh = false
		out[key] = o
		if len(c.at) == 0 && e.states[key] == nil {
			delete(e.counts, key)
		}
	}
}

// observeFacts 把 agent 报告的事实换成观测。调用方不持有 e.mu。
func (e *Engine) observeFacts(ctx context.Context, now time.Time, out map[string]*obs) {
	if e.Probes == nil {
		return
	}
	facts := e.Probes(ctx, now)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range facts {
		set := e.nodeRules()
		if f.Project != "" {
			set = e.forContainer(f.Project, f.Container)
		}
		o := &obs{rule: f.Rule, subject: f.Subject, project: f.Project, set: set, message: f.Message}
		if f.HasValue {
			threshold(o, f.Value)
		} else {
			o.threshold = set.get(f.Rule).GetThreshold()
		}
		if !f.HasValue || f.Judged {
			o.bad, o.good, o.disabled = f.Bad, f.Good, !set.on(f.Rule)
			if !o.bad && !o.good && !o.disabled {
				continue // 没有证据
			}
		}
		out[f.Key] = o
	}
}

// observeHost 判断 inode、conntrack 和网卡错误（监控序列里已有的数据）。
func observeHost(latest map[string]float64, node ruleSet, out map[string]*obs) {
	for series, total := range latest {
		mount, ok := strings.CutSuffix(strings.TrimPrefix(series, "node:filesystem@"), ":inodes_total")
		if !ok || !strings.HasPrefix(series, "node:filesystem@") || total <= 0 {
			continue
		}
		free, ok := latest["node:filesystem@"+mount+":inodes_free"]
		if !ok || free > total {
			continue
		}
		out["inode:"+mount] = threshold(&obs{rule: alert.RuleInode, subject: mount, set: node}, 100*(total-free)/total)
	}
	if n, ok := latest["node:transport:nf_conntrack_count"]; ok {
		if m := latest["node:transport:nf_conntrack_max"]; m > 0 {
			out["conntrack"] = threshold(&obs{rule: alert.RuleConntrack, set: node}, 100*n/m)
		}
	}
	// 网卡：错误和丢包（收发合计）占包数的百分比。虚拟网卡（docker、veth、wg、nagi 等）噪声大，不判断。
	for series, rx := range latest {
		name, ok := strings.CutSuffix(strings.TrimPrefix(series, "node:interface@"), ":rx_packets_per_second")
		if !ok || !strings.HasPrefix(series, "node:interface@") || virtualNIC(name) {
			continue
		}
		p := "node:interface@" + name + ":"
		packets := rx + latest[p+"tx_packets_per_second"]
		if packets < 10 { // 包太少，比例没有意义
			continue
		}
		bad := latest[p+"rx_errors_per_second"] + latest[p+"tx_errors_per_second"] + latest[p+"rx_dropped_per_second"] + latest[p+"tx_dropped_per_second"]
		out["nic_errors:"+name] = threshold(&obs{rule: alert.RuleNIC, subject: name, set: node}, math.Min(100, 100*bad/packets))
	}
}

func virtualNIC(name string) bool {
	for _, p := range []string{"lo", "docker", "br-", "veth", "wg", "nagi", "virbr", "cni", "flannel", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ---- L4（TCP/UDP 映射，见 docs/monitoring.md#tcp--udp-统计） ----

// l4Window 是一条 L4 映射在一个窗口里的统计：新连接数、失败数、平均入出字节/秒。
type l4Window struct {
	conns, fails float64
	in, out      float64
	active       float64 // 最新的活跃连接数
}

// l4Windows 汇总 r:<项目>/<映射>:conn/fail/in/out/active。窗口里要有连续、新鲜的样本，否则不算（缺数据）。
func (e *Engine) l4Windows(ctx context.Context, now time.Time, span time.Duration) map[string]l4Window {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-span), now)
	if err != nil {
		return nil
	}
	out := map[string]l4Window{}
	for series, points := range data {
		route, ok := strings.CutSuffix(strings.TrimPrefix(series, "r:"), ":conn")
		if !ok || len(points) == 0 {
			continue
		}
		previous := now.Add(-span)
		valid := true
		var w l4Window
		for _, p := range points {
			if p.Time.Before(previous) || p.Time.After(now) || p.Time.Sub(previous) > 3*metrics.Interval || p.Value < 0 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				valid = false
				break
			}
			w.conns += p.Value * metrics.Interval.Seconds()
			previous = p.Time
		}
		if !valid || now.Sub(previous) > 3*metrics.Interval {
			continue
		}
		for _, p := range data["r:"+route+":fail"] {
			w.fails += p.Value * metrics.Interval.Seconds()
		}
		for _, k := range []string{"in", "out"} {
			pts := data["r:"+route+":"+k]
			var sum float64
			for _, p := range pts {
				sum += p.Value
			}
			if len(pts) > 0 {
				if k == "in" {
					w.in = sum / float64(len(pts))
				} else {
					w.out = sum / float64(len(pts))
				}
			}
		}
		if a := data["r:"+route+":active"]; len(a) > 0 {
			w.active = a[len(a)-1].Value
		}
		out[route] = w
	}
	return out
}

// observeL4 判断 L4 规则。base 是新连接的基线（l4Baseline），没有的映射不判断突增。
func (e *Engine) observeL4(ctx context.Context, now time.Time, out map[string]*obs) {
	cache := map[time.Duration]map[string]l4Window{}
	window := func(d time.Duration) map[string]l4Window {
		if w, ok := cache[d]; ok {
			return w
		}
		w := e.l4Windows(ctx, now, d)
		cache[d] = w
		return w
	}
	routes := window(alert.ErrorWindowDefault)
	e.mu.Lock()
	sets := map[string]ruleSet{}
	for route := range routes {
		project, name := splitContainer(route)
		sets[route] = e.forRoute(project, name)
	}
	base := e.load.l4Base
	e.mu.Unlock()
	for route, set := range sets {
		project, name := splitContainer(route)
		for _, rule := range []string{alert.RuleL4Surge, alert.RuleL4Active, alert.RuleL4Fail, alert.RuleL4Bandwidth} {
			r := set.get(rule)
			span := time.Duration(max(1, r.GetWindowMinutes())) * time.Minute
			w, ok := window(span)[route]
			if !ok {
				continue
			}
			o := &obs{rule: rule, subject: name, project: project, set: set}
			key := rule + ":" + route
			switch rule {
			case alert.RuleL4Surge:
				b, ok := base[route]
				if !ok || !b.ready || b.rate <= 0 {
					// 基线不足：不判断（当缺数据）；规则关掉时照样结束。
					if !alert.On(r) {
						o.disabled = true
						out[key] = o
					}
					continue
				}
				rate := w.conns / span.Seconds()
				threshold(o, rate/b.rate)
				if rate <= r.GetMinRequests() { // 没到每秒下限不算突增；回到下限以下也算恢复
					o.bad, o.good = false, true
				}
				o.message = l4RateNote(set.lang, rate, b.rate)
			case alert.RuleL4Active:
				threshold(o, w.active)
			case alert.RuleL4Fail:
				if w.conns < r.GetMinRequests() || w.conns == 0 {
					o.lowData, o.disabled = true, !alert.On(r)
					out[key] = o
					continue
				}
				threshold(o, math.Min(100, 100*w.fails/w.conns))
				o.message = connsNote(set.lang, w.conns, span)
			case alert.RuleL4Bandwidth:
				// 入和出各一个键。
				for dir, bps := range map[string]float64{"in": w.in, "out": w.out} {
					x := &obs{rule: rule, subject: name + " " + dirName(set.lang, dir), project: project, set: set}
					out[key+"/"+dir] = threshold(x, bps*8/1e6)
				}
				continue
			}
			out[key] = o
		}
	}
}

func dirName(lang, dir string) string {
	if lang == "en" {
		return map[string]string{"in": "(inbound)", "out": "(outbound)"}[dir]
	}
	return map[string]string{"in": "（入）", "out": "（出）"}[dir]
}

func l4RateNote(lang string, rate, base float64) string {
	if lang == "en" {
		return "Now " + alert.Num(rate) + " new connections/s, baseline " + alert.Num(base) + "/s"
	}
	return "现在每秒 " + alert.Num(rate) + " 个新连接，基线每秒 " + alert.Num(base) + " 个"
}

func connsNote(lang string, n float64, span time.Duration) string {
	if lang == "en" {
		return "Connections in the last " + alert.Duration(span, true) + ": " + alert.Num(n)
	}
	return "最近 " + alert.Duration(span, false) + "共 " + alert.Num(n) + " 个连接"
}

// ---- 基线（见 docs/monitoring.md#基线） ----

// L4BaselineRequired：L4 新连接突增要至少这么多小时的样本才判断。
const L4BaselineRequired = 6 * time.Hour

// baselineWindow 是基线回看的时长（HTTP 请求突增、L4 新连接突增都是 24 小时）。
const baselineWindow = 24 * time.Hour

// routeBaseline 是一条映射的基线：平均速率、样本覆盖的时长、最近一个样本的时间，够不够用。
type routeBaseline struct {
	rate      float64
	collected time.Duration
	last      time.Time
	ready     bool
}

// computeBaselines 从过去 24 小时（不含最近一个窗口）的数据算出每条映射的 HTTP 请求和 L4 新连接基线。
// HTTP 保持原来的规则：有样本且平均值 > 0 就可用；L4 要至少 6 小时的样本。
func (e *Engine) computeBaselines(ctx context.Context, now time.Time) (http, l4 map[string]routeBaseline) {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-baselineWindow), now.Add(-alert.ErrorWindowDefault))
	if err != nil {
		return nil, nil
	}
	http, l4 = map[string]routeBaseline{}, map[string]routeBaseline{}
	for series, points := range data {
		route, kind, ok := cutLast(strings.TrimPrefix(series, "r:"), ":")
		if !ok || len(points) == 0 || kind != "req" && kind != "conn" {
			continue
		}
		var sum float64
		for _, p := range points {
			sum += p.Value
		}
		b := routeBaseline{rate: sum / float64(len(points)), collected: time.Duration(len(points)) * metrics.Interval, last: points[len(points)-1].Time}
		if kind == "req" {
			b.ready = b.rate > 0
			http[route] = b
		} else {
			b.ready = b.rate > 0 && b.collected >= L4BaselineRequired
			l4[route] = b
		}
	}
	return http, l4
}

// baselineStalled：这么久没有新样本就算数据中断。
const baselineStalled = 10 * time.Minute

// Baselines 返回需要先积累历史才能判断的规则的进度（随告警快照上报，见 AlertSync.baselines）。
func (e *Engine) Baselines() []*nagiv1.AlertBaseline {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.baselines()
}

// baselines 同 Baselines。调用方持有 e.mu。
func (e *Engine) baselines() []*nagiv1.AlertBaseline {
	now := e.now()
	var out []*nagiv1.AlertBaseline
	add := func(rule, project, subject string, required, collected time.Duration, last time.Time, ready bool) {
		status := "building"
		switch {
		case ready && collected >= required:
			status = "ready"
		case !last.IsZero() && now.Sub(last) > baselineStalled:
			status = "stalled"
		case ready && rule == alert.RuleTraffic:
			status = "ready" // HTTP 保持原规则：有数据就能判断，进度条只作参考
		}
		b := &nagiv1.AlertBaseline{Rule: rule, ProjectId: project, Subject: subject, RequiredSeconds: uint32(required / time.Second),
			CollectedSeconds: uint32(min(collected, required) / time.Second), Status: status}
		if !last.IsZero() {
			b.LastSample = timestamppb.New(last)
		}
		out = append(out, b)
	}
	for route, b := range e.load.baseline {
		project, name := splitContainer(route)
		if e.forRoute(project, name).on(alert.RuleTraffic) {
			add(alert.RuleTraffic, project, name, baselineWindow, b.collected, b.last, b.ready)
		}
	}
	for route, b := range e.load.l4Base {
		project, name := splitContainer(route)
		if e.forRoute(project, name).on(alert.RuleL4Surge) {
			add(alert.RuleL4Surge, project, name, L4BaselineRequired, b.collected, b.last, b.ready)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectId != out[j].ProjectId {
			return out[i].ProjectId < out[j].ProjectId
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Rule < out[j].Rule
	})
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}

// NodeRule 返回节点生效的一条规则（agent 换算阈值用）。
func (e *Engine) NodeRule(rule string) *nagiv1.AlertRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodeRules().get(rule)
}

// ContainerRule 返回容器生效的一条规则。
func (e *Engine) ContainerRule(projectID, container, rule string) *nagiv1.AlertRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forContainer(projectID, container).get(rule)
}

// FiringKeys 返回以 prefix 开头、正在告警中的键。
func (e *Engine) FiringKeys(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for k, st := range e.states {
		if st.firing && strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}
