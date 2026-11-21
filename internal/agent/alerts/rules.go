package alerts

import (
	"context"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 生效规则（见 docs/monitoring.md#告警）：面板算好每条规则的每个字段下发（AlertConfig.effective、
// ContainerSpec.effective_alert_rules、LinkEntry.effective_alert_rules）。连着旧版本面板时只有旧的 AlertRules，
// 按 alert.LegacyEffective 换算。几个面板管着同一个节点时取最严格的。

// ruleSet 是一个节点或容器生效的规则。
type ruleSet struct {
	rules      map[string]*nagiv1.AlertRule
	// off 是关掉的事件通知（见 alert.Notices）。
	off        map[string]bool
	lang       string
	flapWindow time.Duration
	flapCount  int
}

func (s ruleSet) get(name string) *nagiv1.AlertRule { return alert.Rule(s.rules, name) }
func (s ruleSet) on(name string) bool               { return alert.On(s.get(name)) }

func fromEffective(eff *nagiv1.EffectiveAlertRules) ruleSet {
	s := ruleSet{rules: eff.GetRules(), off: eff.GetNotifications(), lang: alert.Language(eff.GetLanguage()),
		flapWindow: time.Duration(eff.GetFlapWindowMinutes()) * time.Minute, flapCount: int(eff.GetFlapCount())}
	if s.flapWindow <= 0 {
		s.flapWindow = alert.DefaultFlapWindowMinutes * time.Minute
	}
	if s.flapCount <= 0 {
		s.flapCount = alert.DefaultFlapCount
	}
	return s
}

// panelRules 是一个面板下发的节点规则。
func panelRules(cfg *nagiv1.AlertConfig) ruleSet {
	if eff := cfg.GetEffective(); eff != nil {
		return fromEffective(eff)
	}
	return fromEffective(alert.LegacyEffective(cfg.GetRules(), true))
}

// nodeRules 合并所有面板的节点规则（取最严格的）。调用方持有 e.mu。
func (e *Engine) nodeRules() ruleSet {
	var out ruleSet
	first := true
	for _, p := range e.panelOrder() {
		x := panelRules(e.configs[p])
		if first {
			out, first = x, false
			continue
		}
		out = strictest(out, x)
	}
	if first {
		out = fromEffective(alert.Effective(nil, true))
	}
	return out
}

func strictest(a, b ruleSet) ruleSet {
	// 事件通知：任何一个面板开着就发（只有都关掉了才不发）。
	off := map[string]bool{}
	for k := range a.off {
		if _, ok := b.off[k]; ok {
			off[k] = false
		}
	}
	out := ruleSet{rules: map[string]*nagiv1.AlertRule{}, off: off, lang: a.lang, flapWindow: max(a.flapWindow, b.flapWindow), flapCount: min(a.flapCount, b.flapCount)}
	for _, d := range alert.Defs {
		if a.rules[d.Name] == nil && b.rules[d.Name] == nil {
			continue
		}
		out.rules[d.Name] = alert.Strictest(a.rules[d.Name], b.rules[d.Name], d.Lower)
	}
	return out
}

// containerRuleSet 是容器（或映射所属容器）的规则：面板随容器配置下发的；没有下发时用节点配置里的（旧版本面板）。
func (e *Engine) containerRuleSet(eff *nagiv1.EffectiveAlertRules, legacy *nagiv1.AlertRules, ok bool, fallback ruleSet) ruleSet {
	switch {
	case eff != nil:
		s := fromEffective(eff)
		s.lang = fallback.lang
		return s
	case ok:
		s := fromEffective(alert.LegacyEffective(legacy, false))
		s.lang, s.flapWindow, s.flapCount = fallback.lang, fallback.flapWindow, fallback.flapCount
		return s
	}
	return fallback
}

// legacyContainer 是旧版本面板放在节点告警配置里的容器规则（v0.13 以前全局规则）。调用方持有 e.mu。
func (e *Engine) legacyContainer() ruleSet {
	var out ruleSet
	first := true
	for _, p := range e.panelOrder() {
		x := fromEffective(alert.LegacyEffective(e.configs[p].GetRules(), false))
		if first {
			out, first = x, false
			continue
		}
		out = strictest(out, x)
	}
	if first {
		out = fromEffective(alert.Effective(nil, false))
	}
	n := e.nodeRules()
	out.lang, out.flapWindow, out.flapCount = n.lang, n.flapWindow, n.flapCount
	return out
}

// forContainer 返回容器的规则。调用方持有 e.mu（ContainerRules 只读节点的容器配置，不回调引擎）。
func (e *Engine) forContainer(projectID, container string) ruleSet {
	fallback := e.legacyContainer()
	if e.ContainerRules == nil {
		return fallback
	}
	eff, legacy, ok := e.ContainerRules(projectID, container)
	return e.containerRuleSet(eff, legacy, ok, fallback)
}

// forRoute 返回 http 映射所属容器的规则。调用方持有 e.mu。
func (e *Engine) forRoute(projectID, route string) ruleSet {
	fallback := e.legacyContainer()
	if e.RouteRules == nil {
		return fallback
	}
	eff, legacy, ok := e.RouteRules(projectID, route)
	return e.containerRuleSet(eff, legacy, ok, fallback)
}

// NodeEnabled 判断节点的规则开着没有，给事件类告警（证书申请失败）用。
func (e *Engine) NodeEnabled(rule string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodeRules().on(rule)
}

// ContainerEnabled 判断容器的规则开着没有，给事件类告警（快照失败、备份同步失败、出站拦截）用。
func (e *Engine) ContainerEnabled(projectID, container, rule string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forContainer(projectID, container).on(rule)
}

// EgressRepeat 是出站拦截的去重时间：同一个容器、同一个目标这么久之内只告警一次。
func (e *Engine) EgressRepeat(projectID, container string) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.forContainer(projectID, container).get(alert.RuleEgress).GetRepeatMinutes()
	if m == 0 {
		return 0
	}
	return time.Duration(m) * time.Minute
}

// ruleOf 返回告警键对应的规则和它所属的容器、映射（用于取规则）。
func ruleOf(key string) (rule, project, object string) {
	kind, rest, _ := strings.Cut(key, ":")
	switch kind {
	case "event":
		kind, rest, _ = strings.Cut(rest, ":")
		switch kind {
		case "cert_failed":
			return alert.RuleCert, "", ""
		case "snapshot", "maintain":
			p, c, _ := strings.Cut(rest, "/")
			return alert.RuleSnapshot, p, c
		case "backup", "standby":
			p, c, _ := strings.Cut(rest, "/")
			return alert.RuleBackup, p, c
		}
		return "", "", ""
	case "disk":
		return alert.RuleDisk, "", ""
	case alert.RuleMem, alert.RuleCPU:
		return kind, "", ""
	case "cert":
		return alert.RuleCert, "", ""
	case "errors", "4xx", "traffic":
		p, r := splitContainer(rest)
		return kind, p, r
	case "health", "restart", "container_cpu", alert.RuleContainerOOM, alert.RuleSnapshotStale, alert.RuleBackupLag:
		p, c := splitContainer(rest)
		c, _, _ = strings.Cut(c, "/")
		return kind, p, c
	}
	if d, ok := alert.DefOf(kind); ok {
		if d.Route { // l4_*:<项目>/<映射>[/in|out]
			p, r := splitContainer(rest)
			r, _, _ = strings.Cut(r, "/")
			return kind, p, r
		}
		if d.Node {
			return kind, "", ""
		}
	}
	return "", "", ""
}

// rulesFor 返回告警键所用的规则集。调用方持有 e.mu。
func (e *Engine) rulesFor(key string) (string, ruleSet) {
	rule, p, o := ruleOf(key)
	d, _ := alert.DefOf(rule)
	switch {
	case d.Route:
		return rule, e.forRoute(p, o)
	case rule != "" && !d.Node:
		if p != "" {
			return rule, e.forContainer(p, o)
		}
		return rule, e.legacyContainer()
	}
	return rule, e.nodeRules()
}

// ruleEnabled 判断告警键的规则还开着没有（补发队列里的出现通知用，规则关了就不再发）。调用方持有 e.mu。
// 不再使用的域名的证书告警：缺席不是健康，也不算规则关掉，照常允许发送。
func (e *Engine) ruleEnabled(key string) bool {
	if d, ok := strings.CutPrefix(key, "cert:"); ok && e.Domains != nil && !e.Domains()[d] {
		return true
	}
	rule, set := e.rulesFor(key)
	return rule == "" || set.on(rule)
}

// NotifyEgress 发一条出站被拦截的通知（一次性，见 docs/agent.md#出站限制）：规则关着不发；同一个容器、同一个目标
// 在规则的提醒间隔内只发一次（间隔为 0 时只按出站限制自己的 10 分钟去重）。
func (e *Engine) NotifyEgress(ctx context.Context, project, container, target, title string) {
	key := "egress:" + project + "/" + container + "/" + target
	now := time.Now()
	e.mu.Lock()
	set := e.forContainer(project, container)
	every := time.Duration(set.get(alert.RuleEgress).GetRepeatMinutes()) * time.Minute
	if !set.on(alert.RuleEgress) || every > 0 && now.Sub(e.egressSent[key]) < every || now.Before(e.mutedKeys[key]) {
		e.mu.Unlock()
		return
	}
	if e.egressSent == nil || len(e.egressSent) > 10000 {
		e.egressSent = map[string]time.Time{}
	}
	e.egressSent[key] = now
	e.mu.Unlock()
	e.NotifyKey(ctx, key, title, "", project)
}
