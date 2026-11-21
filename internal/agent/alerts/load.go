package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// CPU 和流量规则只报告单项事实，不从 CPU、请求量或 4xx 推断攻击原因。
// 阈值沿用现有默认值，规则可以在节点和项目的告警规则里关掉。

const (
	cpuPercent    = 90.0
	cpuFor        = 10 * time.Minute
	surgeFactor   = 10.0
	surgeMinRate  = 50.0 // 请求/秒
	clientPercent = 50.0
	clientMinReq  = 200.0
	// baselineRefresh：过去 24 小时的平均请求量隔这么久重算一次。
	baselineRefresh = 5 * time.Minute
)

// loadState 是 CPU 和流量规则的状态。
type loadState struct {
	cpuSince   time.Time
	ctrSince   map[string]time.Time
	baseline   map[string]float64 // 规则 → 过去 24 小时的平均请求量（请求/秒）
	baselineAt time.Time
}

// evaluateLoad 判断 CPU 和流量规则，把出现的告警放进 active。node 是节点自己的规则，调用方持有的锁已释放。
func (e *Engine) evaluateLoad(ctx context.Context, now time.Time, latest map[string]float64, node judge, enabled map[string]bool, active map[string]*nagiv1.Alert, healthy map[string]bool) {
	rates := e.routeRates(ctx, now)
	if now.Sub(e.load.baselineAt) >= baselineRefresh {
		e.load.baseline, e.load.baselineAt = e.baselines(ctx, now), now
	}
	e.judgeLoad(now, latest, rates, node, enabled, active, healthy)
}

// judgeLoad 按最新的指标、最近 5 分钟的请求量和基线判断。
func (e *Engine) judgeLoad(now time.Time, latest map[string]float64, rates map[string]routeRate, node judge, enabled map[string]bool, active map[string]*nagiv1.Alert, healthy map[string]bool) {
	st := &e.load
	// 节点 CPU 持续过高。
	if v, ok := latest["node:cpu"]; ok && v > cpuPercent && enabled[alert.RuleCPU] {
		if st.cpuSince.IsZero() {
			st.cpuSince = now
		}
		if now.Sub(st.cpuSince) >= cpuFor {
			active["cpu"] = &nagiv1.Alert{Title: fmt.Sprintf("CPU 使用 %.0f%%，已持续超过 %d 分钟", v, int(cpuFor.Minutes()))}
		}
	} else {
		st.cpuSince = time.Time{}
		if v, ok := latest["node:cpu"]; ok && v <= cpuPercent && enabled[alert.RuleCPU] {
			healthy["cpu"] = true
		}
	}

	// 容器 CPU 持续超过限额（没有限额时是整机）的 90%：各服务加起来和 l:<项目>/<容器>:cpu 比。
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
	if st.ctrSince == nil {
		st.ctrSince = map[string]time.Time{}
	}
	for key, v := range used {
		limit := latest["l:"+key+":cpu"]
		project, container, _ := strings.Cut(key, "/")
		if limit <= 0 || v < limit*cpuPercent/100 || !e.forContainer(project, container, node).on(alert.RuleContainerCPU) {
			delete(st.ctrSince, key)
			if limit > 0 && v < limit*cpuPercent/100 && e.forContainer(project, container, node).on(alert.RuleContainerCPU) {
				healthy["container_cpu:"+key] = true
			}
			continue
		}
		if st.ctrSince[key].IsZero() {
			st.ctrSince[key] = now
		}
		if now.Sub(st.ctrSince[key]) >= cpuFor {
			active["container_cpu:"+key] = &nagiv1.Alert{
				Title:   fmt.Sprintf("容器 %s 的 CPU 使用 %.0f%%（上限 %.0f%%），已持续超过 %d 分钟", container, v, limit, int(cpuFor.Minutes())),
				Project: e.projectName(project),
			}
		}
	}
	for key := range st.ctrSince {
		if _, ok := used[key]; !ok {
			delete(st.ctrSince, key)
		}
	}

	// 请求量突增、4xx 比例过高：按 http 规则算最近 5 分钟。
	for route, x := range rates {
		project, name := splitContainer(route)
		j := node
		if e.RouteRules != nil {
			r, ok := e.RouteRules(project, name)
			j = forRules(r, ok, node)
		}
		window := alert.ErrorWindow.Seconds()
		rate := x.requests / window
		if base := st.baseline[route]; rate > surgeMinRate && base > 0 && rate > surgeFactor*base && j.on(alert.RuleTraffic) {
			active["traffic:"+route] = &nagiv1.Alert{
				Title:   fmt.Sprintf("规则 %s 最近 %d 分钟每秒 %.0f 个请求，是过去 24 小时平均的 %.0f 倍", name, int(alert.ErrorWindow.Minutes()), rate, rate/base),
				Project: e.projectName(project),
			}
		} else if j.on(alert.RuleTraffic) && (rate <= surgeMinRate || st.baseline[route] > 0) {
			healthy["traffic:"+route] = true
		}
		if !x.missingClient && x.requests >= clientMinReq && 100*x.client/x.requests > clientPercent && j.on(alert.RuleClientErrors) {
			active["4xx:"+route] = &nagiv1.Alert{
				Title:   fmt.Sprintf("规则 %s 最近 %d 分钟 4xx 占 %.0f%%（共 %.0f 个请求）", name, int(alert.ErrorWindow.Minutes()), 100*x.client/x.requests, x.requests),
				Project: e.projectName(project),
			}
		} else if !x.missingClient && x.requests >= clientMinReq && j.on(alert.RuleClientErrors) {
			healthy["4xx:"+route] = true
		}
	}
}

type routeRate struct {
	requests, client float64
	missingClient    bool
}

// routeRates 统计每条 http 规则最近 5 分钟的请求数和 4xx 数。
func (e *Engine) routeRates(ctx context.Context, now time.Time) map[string]routeRate {
	out := map[string]routeRate{}
	for route, x := range e.windowRates(ctx, now, "req") {
		out[route] = routeRate{requests: x.requests, missingClient: true}
	}
	for route, x := range e.windowRates(ctx, now, "4xx") {
		out[route] = routeRate{requests: x.requests, client: x.errors}
	}
	return out
}

// baselines 算每条 http 规则过去 24 小时的平均请求量（请求/秒），不算最近 5 分钟。
func (e *Engine) baselines(ctx context.Context, now time.Time) map[string]float64 {
	data, err := e.metrics.Query(ctx, "r:", now.Add(-24*time.Hour), now.Add(-alert.ErrorWindow))
	if err != nil {
		return nil
	}
	out := map[string]float64{}
	for series, points := range data {
		route, kind, ok := cutLast(strings.TrimPrefix(series, "r:"), ":")
		if !ok || kind != "req" || len(points) == 0 {
			continue
		}
		var sum float64
		for _, p := range points {
			sum += p.Value
		}
		out[route] = sum / float64(len(points))
	}
	return out
}
