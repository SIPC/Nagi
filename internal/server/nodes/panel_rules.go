package nodes

import (
	"context"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 面板判断的规则（见 docs/monitoring.md#告警）：节点时钟偏差、节点升级失败或回退、审计链校验、令牌到期。
// 和节点上一样：超过触发阈值持续 for_seconds 才告警；回到恢复阈值以内保持 recover_seconds 才恢复；
// 告警中按 repeat_minutes 提醒；规则关掉、对象不在了以 rule_disabled / object_removed 结束，不通知。
// 只在主面板上判断（备用面板不发告警）。

// PanelObservation 是一个告警键这一轮的观测。
type PanelObservation struct {
	Key, Rule, Node, Subject string
	// NodeID 不为空时用节点的规则（含节点覆盖），否则用全局的。
	NodeID string
	Value  *float64
	// Bad、Good：超过触发阈值、回到恢复阈值以内（都为 false 是介于两者之间）。
	Bad, Good bool
	Message   string
}

type panelTrack struct {
	badSince, goodSince, notified, since time.Time
	firing                               bool
	peak                                 float64
}

type panelRules struct {
	mu     sync.Mutex
	tracks map[string]*panelTrack
}

// EvaluatePanelRule 推进一个告警键的状态。obs 为 nil 表示没有数据（不动）。
func (r *Registry) EvaluatePanelRule(ctx context.Context, o PanelObservation, now time.Time) {
	if r.ReadOnly {
		return
	}
	policy, err := r.AlertPolicy(ctx)
	if err != nil {
		return
	}
	rule := alert.Merge(policy.GetRules())[o.Rule]
	if o.NodeID != "" {
		if eff, err := r.NodeAlertEffective(ctx, o.NodeID); err == nil {
			rule = alert.Rule(eff.GetRules(), o.Rule)
		}
	}
	lang := alert.Language(policy.GetLanguage())
	p := &r.panelRules
	p.mu.Lock()
	if p.tracks == nil {
		p.tracks = map[string]*panelTrack{}
	}
	t := p.tracks[o.Key]
	if t == nil {
		t = &panelTrack{}
		p.tracks[o.Key] = t
	}
	var send *nagiv1.Alert
	base := func(status string) *nagiv1.Alert {
		a := &nagiv1.Alert{Key: o.Key, Status: status, Rule: o.Rule, Node: o.Node, Subject: o.Subject, Language: lang, Time: timestamppb.New(now), Message: o.Message, Value: o.Value}
		if rule.Threshold != nil {
			th, rec := rule.GetThreshold(), rule.GetRecoverThreshold()
			a.Threshold, a.RecoverThreshold = &th, &rec
		}
		if t.firing && o.Value != nil {
			pk := t.peak
			a.Peak = &pk
		}
		return a
	}
	switch {
	case !alert.On(rule):
		if t.firing {
			send = base("ended")
			send.EndReason = "rule_disabled"
		}
		delete(p.tracks, o.Key)
	case !t.firing:
		t.goodSince = time.Time{}
		if !o.Bad {
			t.badSince = time.Time{}
			break
		}
		if t.badSince.IsZero() {
			t.badSince = now
		}
		if now.Sub(t.badSince) < time.Duration(rule.GetForSeconds())*time.Second {
			break
		}
		t.firing, t.since, t.notified = true, t.badSince, now
		if o.Value != nil {
			t.peak = *o.Value
		}
		send = base("firing")
		send.DurationSeconds = uint32(now.Sub(t.since) / time.Second)
	default:
		if o.Value != nil && *o.Value > t.peak {
			t.peak = *o.Value
		}
		if o.Good {
			if t.goodSince.IsZero() {
				t.goodSince = now
			}
			if now.Sub(t.goodSince) >= time.Duration(rule.GetRecoverSeconds())*time.Second {
				send = base("resolved")
				send.DurationSeconds, send.RecoverSeconds = uint32(now.Sub(t.since)/time.Second), uint32(now.Sub(t.goodSince)/time.Second)
				delete(p.tracks, o.Key)
			}
			break
		}
		t.goodSince = time.Time{}
		if every := time.Duration(rule.GetRepeatMinutes()) * time.Minute; every > 0 && now.Sub(t.notified) >= every {
			t.notified = now
			send = base("firing")
			send.Reminder, send.DurationSeconds = true, uint32(now.Sub(t.since)/time.Second)
		}
	}
	p.mu.Unlock()
	if send == nil {
		return
	}
	send.Title, _ = alert.Render(withLang(send, "zh-CN"))
	if send.Status == "ended" {
		if _, err := r.trackPanelAlert(ctx, send); err != nil {
			r.log.Warn("保存告警快照失败", "err", err)
		}
		return
	}
	r.sendLogged(ctx, send)
}

// EndPanelRule 在对象不在了（节点被移除、令牌被删除）时结束这个键的告警，不通知。
func (r *Registry) EndPanelRule(ctx context.Context, key string) {
	p := &r.panelRules
	p.mu.Lock()
	t := p.tracks[key]
	delete(p.tracks, key)
	p.mu.Unlock()
	if t != nil && t.firing {
		if _, err := r.trackPanelAlert(ctx, &nagiv1.Alert{Key: key, Status: "ended", EndReason: "object_removed"}); err != nil {
			r.log.Warn("保存告警快照失败", "err", err)
		}
	}
}

// PanelRuleKeys 返回正在跟踪的、以 prefix 开头的键。
func (r *Registry) PanelRuleKeys(prefix string) []string {
	p := &r.panelRules
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.tracks {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	return out
}

func withLang(a *nagiv1.Alert, lang string) *nagiv1.Alert {
	x := proto.CloneOf(a)
	x.Language = lang
	return x
}

// checkClock 按节点上报里的本机时间判断时钟偏差（v0.37.1 起节点才带 node_time；没有时不判断）。
// 上报在路上的时间算进偏差里，所以阈值不要低于网络延迟；默认 5 秒远大于正常的延迟。
func (r *Registry) checkClock(ctx context.Context, c *Conn, nodeTime, received time.Time) {
	skew := received.Sub(nodeTime).Seconds()
	if skew < 0 {
		skew = -skew
	}
	policy, err := r.AlertPolicy(ctx)
	if err != nil {
		return
	}
	rule := alert.Merge(policy.GetRules())[alert.RuleClockSkew]
	if eff, err := r.NodeAlertEffective(ctx, c.NodeID); err == nil {
		rule = alert.Rule(eff.GetRules(), alert.RuleClockSkew)
	}
	v := skew
	r.EvaluatePanelRule(ctx, PanelObservation{Key: "clock_skew:" + c.NodeID, Rule: alert.RuleClockSkew, NodeID: c.NodeID, Node: c.Name(),
		Value: &v, Bad: skew > rule.GetThreshold(), Good: alert.Def{}.Recovered(skew, rule.GetRecoverThreshold())}, received)
}
