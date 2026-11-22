package alerts

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 规则告警的状态机（见 docs/monitoring.md#告警）。每个告警键一份：
//
//	正常 ──超过触发阈值──▶ 待触发 ──持续满 for_seconds──▶ 告警中（发出现通知）
//	待触发 ──回到触发阈值以内──▶ 正常（不发任何通知）
//	告警中 ──回到恢复阈值以内──▶ 恢复中 ──保持满 recover_seconds──▶ 正常（发恢复通知，结束这一轮）
//	恢复中 ──又超过恢复阈值──▶ 告警中（不发通知，不算新的一轮）
//
// 缺数据（没有样本、样本过期、查询失败）时状态和计时都不动；连续缺数据超过 staleAfter 才把待触发、恢复中的计时清零，
// 但不会因此判定恢复。告警中关掉规则以 rule_disabled 结束，对象不在了以 object_removed 结束。

// obs 是一次判断里一个告警键的观测。
type obs struct {
	rule    string
	subject string
	project string // 项目 ID（容器的规则）
	// bad：超过触发阈值；good：回到恢复阈值以内。都为 false 是介于两者之间（只有回差区间里会这样）。
	bad, good bool
	value     float64
	hasValue  bool
	// threshold、recover 是这次判断用的阈值。
	threshold, recover float64
	message            string
	// disabled：规则关着（这一轮告警以 rule_disabled 结束）。
	disabled bool
	// lowData：请求数不够，不能判断（5xx、4xx），持续 InsufficientDataAfter 后以 insufficient_data 结束。
	lowData bool
	set     ruleSet
}

// staleAfter：连续这么久没有数据，就把待触发、恢复中的计时清零。
const staleAfter = 90 * time.Second

// track 是一个告警键在状态机里的计时（不保存：重启后重新计时，宁可晚恢复，也不误报恢复）。
type track struct {
	badSince  time.Time // 进入待触发的时间
	goodSince time.Time // 进入恢复中的时间
	seen      time.Time // 最近一次有数据
	lowSince  time.Time // 请求数一直不够的开始时间
}

// flapState 是一个告警键最近几轮出现的时间，和是否处于抖动中。
type flapState struct {
	starts  []time.Time
	active  bool
	since   time.Time // 进入抖动或最近一次状态变化
	firing  bool      // 最近一次状态（抖动期间不通知，只记下）
	changed time.Time
}

// step 推进一个告警键的状态机，返回要发送的通知（可以是 nil）。调用方持有 e.mu。
func (e *Engine) step(key string, o *obs, now time.Time) *nagiv1.Alert {
	tr := e.tracks[key]
	if tr == nil {
		tr = &track{}
		e.tracks[key] = tr
	}
	st := e.states[key]
	firing := st != nil && st.firing
	if o == nil { // 没有数据
		if !tr.seen.IsZero() && now.Sub(tr.seen) > staleAfter {
			tr.badSince, tr.goodSince = time.Time{}, time.Time{}
		}
		if st != nil {
			st.confirmed = false
		}
		return nil
	}
	tr.seen = now
	if o.disabled {
		tr.badSince, tr.goodSince, tr.lowSince = time.Time{}, time.Time{}, time.Time{}
		if firing {
			return e.end(key, "rule_disabled")
		}
		return nil
	}
	r := o.set.get(o.rule)
	if o.lowData {
		if st != nil {
			st.confirmed = false
		}
		if firing {
			if tr.lowSince.IsZero() {
				tr.lowSince = now
			}
			if now.Sub(tr.lowSince) >= alert.InsufficientDataAfter {
				return e.end(key, "insufficient_data")
			}
		}
		return nil
	}
	tr.lowSince = time.Time{}
	if !firing {
		tr.goodSince = time.Time{}
		if !o.bad {
			tr.badSince = time.Time{}
			return nil
		}
		if tr.badSince.IsZero() {
			tr.badSince = now
		}
		hold := time.Duration(r.GetForSeconds()) * time.Second
		if d, _ := alert.DefOf(o.rule); d.Unit == "seconds" && !d.Has(alert.FieldFor) {
			hold = time.Duration(r.GetThreshold()) * time.Second // mesh 成员断开、见证人失联：阈值就是要持续多久
		}
		if now.Sub(tr.badSince) < hold {
			return nil
		}
		return e.fireRule(key, o, now, now.Sub(tr.badSince))
	}
	// 告警中
	st.confirmed = true
	if o.hasValue {
		st.updatePeak(o)
	}
	if o.good {
		if tr.goodSince.IsZero() {
			tr.goodSince = now
		}
		if now.Sub(tr.goodSince) >= time.Duration(r.GetRecoverSeconds())*time.Second {
			a := e.end(key, "recovered")
			a.Value, a.RecoverSeconds = valuePtr(o), uint32(now.Sub(tr.goodSince)/time.Second)
			tr.badSince, tr.goodSince = time.Time{}, time.Time{}
			return a
		}
		return nil
	}
	// 超过恢复阈值（包括回差区间里）：仍在告警中，更新数值，按间隔提醒。
	tr.goodSince = time.Time{}
	st.updateValue(o)
	return e.remind(key, st, r, now)
}

func valuePtr(o *obs) *float64 {
	if !o.hasValue {
		return nil
	}
	v := o.value
	return &v
}

// updateValue 更新告警中的当前值（面板上看到的是最新的值，通知只在出现和提醒时发）。
func (s *state) updateValue(o *obs) {
	if s.alert == nil || !o.hasValue {
		return
	}
	s.alert.Value = valuePtr(o)
	s.alert.Message = o.message
	s.updatePeak(o)
}

func (s *state) updatePeak(o *obs) {
	if s.alert == nil || !o.hasValue {
		return
	}
	d, _ := alert.DefOf(o.rule)
	if s.alert.Peak == nil || !d.Lower && o.value > s.alert.GetPeak() || d.Lower && o.value < s.alert.GetPeak() {
		s.alert.Peak = valuePtr(o)
	}
}

// fireRule 开始新的一轮规则告警。
func (e *Engine) fireRule(key string, o *obs, now time.Time, over time.Duration) *nagiv1.Alert {
	a := &nagiv1.Alert{Key: key, Rule: o.rule, Subject: o.subject, Message: o.message, Value: valuePtr(o),
		Peak: valuePtr(o), DurationSeconds: uint32(over / time.Second)}
	if o.set.get(o.rule).Threshold != nil {
		a.Threshold, a.RecoverThreshold = &o.threshold, &o.recover
	}
	if o.project != "" {
		a.Project = e.projectName(o.project)
	}
	a.Title = legacyTitle(a)
	return e.setFiring(key, a, o.set, now)
}

// legacyTitle 是存进告警记录的中文标题（日志页、活动记录、旧版本面板显示它）。
func legacyTitle(a *nagiv1.Alert) string {
	zh := proto.CloneOf(a)
	zh.Language, zh.Status = "zh-CN", "firing"
	t, _ := alert.Render(zh)
	return t
}

// remind 在告警中按间隔发重复提醒。
func (e *Engine) remind(key string, st *state, r *nagiv1.AlertRule, now time.Time) *nagiv1.Alert {
	every := time.Duration(r.GetRepeatMinutes()) * time.Minute
	if every <= 0 || now.Sub(st.lastNotified) < every || e.flapActive(key) {
		return nil
	}
	if st.lastDelivered.IsZero() && e.notificationPending(st.occurrence) {
		return nil // 第一条还没发出去，补发队列会发
	}
	st.lastNotified = now
	st.alert.DurationSeconds = uint32(now.Sub(st.since) / time.Second)
	a := proto.CloneOf(st.alert)
	a.Time, a.Reminder = timestamppb.New(now), true
	return a
}

func (e *Engine) notificationPending(occurrence string) bool {
	for _, q := range e.queue {
		if q.Source == e.source && q.Occurrence == occurrence {
			return true
		}
	}
	return false
}

// setFiring 开始或继续一轮告警，需要通知时返回要发送的告警。规则告警由状态机保证只在开始时调用一次；
// 事件类告警每次失败都会调用，在重复提醒间隔内不再通知。调用方持有 e.mu。
func (e *Engine) setFiring(key string, a *nagiv1.Alert, set ruleSet, now time.Time) *nagiv1.Alert {
	st := e.states[key]
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
	a.Language = set.lang
	if st.firing {
		every := alert.RepeatInterval // 没有规则的事件（顶替、租约等）
		if rule := ruleOfKey(key); rule != "" {
			every = time.Duration(set.get(rule).GetRepeatMinutes()) * time.Minute
		}
		if !st.lastDelivered.IsZero() || e.notificationPending(st.occurrence) || len(e.channels()) == 0 {
			if every <= 0 || now.Sub(st.lastNotified) < every {
				st.alert = a
				return nil
			}
			a.Reminder = true
		}
	}
	reminder := a.Reminder
	a.Reminder = false
	st.firing, st.lastNotified, st.alert = true, now, a
	if !reminder && e.flapping(key, now, set, true) {
		return nil
	}
	a = proto.CloneOf(a)
	a.Reminder = reminder
	return a
}

func ruleOfKey(key string) string {
	r, _, _ := ruleOf(key)
	return r
}

// flapping 记下一次状态变化，返回这次要不要压住通知。进入抖动时发一条「反复出现」的通知（由调用方发送队列取走）。
func (e *Engine) flapping(key string, now time.Time, set ruleSet, starting bool) bool {
	if isEventKey(key) {
		return false
	}
	f := e.flaps[key]
	if f == nil {
		f = &flapState{}
		e.flaps[key] = f
	}
	f.firing, f.changed = starting, now
	if starting {
		kept := f.starts[:0]
		for _, t := range f.starts {
			if now.Sub(t) < set.flapWindow {
				kept = append(kept, t)
			}
		}
		f.starts = append(kept, now)
	}
	if f.active {
		return true
	}
	if starting && len(f.starts) >= set.flapCount {
		f.active, f.since = true, now
		st := e.states[key]
		n := &nagiv1.Alert{Key: key, Status: "flapping", Node: e.nodeName(), Time: timestamppb.New(now), Rule: ruleOfKey(key),
			Language: set.lang, Source: e.source}
		if st != nil {
			n.Occurrence, n.Project, n.Subject = st.occurrence, st.alert.GetProject(), st.alert.GetSubject()
		}
		n.Title, n.Message = flapText(st, set, len(f.starts))
		e.notices = append(e.notices, n)
		return true
	}
	return false
}

// flapText 是「反复出现」通知的文字。
func flapText(st *state, set ruleSet, n int) (string, string) {
	subject := ""
	if st != nil && st.alert != nil {
		x := proto.CloneOf(st.alert)
		x.Language, x.Status, x.Reminder = set.lang, "firing", false
		subject, _ = alert.Render(x)
	}
	mins := int(set.flapWindow / time.Minute)
	if set.lang == "en" {
		return fmt.Sprintf("Flapping: fired %d times in %d min, pausing per-event notifications", n, mins),
			subject + fmt.Sprintf("\nNotifications resume after it stays firing or resolved for %d min", alert.FlapQuietMinutes)
	}
	return fmt.Sprintf("反复出现：%d 分钟内触发了 %d 次，已暂停逐条通知", mins, n),
		subject + fmt.Sprintf("\n保持告警或保持正常 %d 分钟后恢复逐条通知", alert.FlapQuietMinutes)
}

// settleFlaps 检查抖动中的告警键：保持同一个状态 FlapQuietMinutes 后退出抖动；退出时还在告警中，补发一条出现通知。
// 调用方持有 e.mu。
func (e *Engine) settleFlaps(now time.Time) []*nagiv1.Alert {
	var out []*nagiv1.Alert
	for key, f := range e.flaps {
		if !f.active {
			if len(f.starts) == 0 || now.Sub(f.starts[len(f.starts)-1]) > 24*time.Hour {
				delete(e.flaps, key)
			}
			continue
		}
		if now.Sub(f.changed) < alert.FlapQuietMinutes*time.Minute {
			continue
		}
		f.active, f.starts = false, nil
		if st := e.states[key]; st != nil && st.firing {
			st.lastNotified = now
			st.alert.DurationSeconds = uint32(now.Sub(st.since) / time.Second)
			out = append(out, proto.CloneOf(st.alert))
		}
	}
	return out
}

// end 结束一轮告警，返回要发送的通知（恢复或结束）。抖动中的恢复不发送。调用方持有 e.mu。
func (e *Engine) end(key, reason string) *nagiv1.Alert {
	st := e.states[key]
	now := e.now()
	status := "ended"
	if reason == "recovered" {
		status = "resolved"
	}
	a := &nagiv1.Alert{Key: key, Status: status, Title: st.alert.GetTitle(), Project: st.alert.GetProject(),
		Node: e.nodeName(), Time: timestamppb.New(now), Occurrence: st.occurrence, Source: e.source,
		Since: timestamppb.New(st.since), EndReason: reason, Validity: "unknown",
		Rule: st.alert.GetRule(), Subject: st.alert.GetSubject(), Threshold: st.alert.Threshold, RecoverThreshold: st.alert.RecoverThreshold,
		Peak: st.alert.Peak, DurationSeconds: uint32(now.Sub(st.since) / time.Second), Language: st.alert.GetLanguage()}
	if reason == "recovered" {
		a.Validity = "valid"
	} else {
		a.Title, a.Message = endText(st.alert, reason)
	}
	e.history = append(e.history, a)
	if len(e.history) > 200 {
		e.history = e.history[len(e.history)-200:]
	}
	delete(e.states, key)
	if f := e.flaps[key]; f != nil {
		f.firing, f.changed = false, now
	}
	return a
}

// endText 是没有恢复就结束的通知文字。
func endText(a *nagiv1.Alert, reason string) (string, string) {
	title, _ := alert.Render(withStatus(a, "firing"))
	en := a.GetLanguage() == "en"
	switch reason {
	case "rule_disabled":
		if en {
			return title, "The rule was turned off; this is not a recovery"
		}
		return title, "规则已关闭，这不是恢复"
	case "object_removed":
		if en {
			return title, "The object no longer exists; this is not a recovery"
		}
		return title, "对象已不存在，这不是恢复"
	case "insufficient_data":
		if en {
			return title, fmt.Sprintf("Too few requests for %d min to tell; this is not a recovery", int(alert.InsufficientDataAfter/time.Minute))
		}
		return title, fmt.Sprintf("请求太少，已 %d 分钟无法判断，这不是恢复", int(alert.InsufficientDataAfter/time.Minute))
	}
	return title, ""
}

func withStatus(a *nagiv1.Alert, status string) *nagiv1.Alert {
	x := proto.CloneOf(a)
	x.Status = status
	return x
}

// flapActive 判断告警键在不在抖动中（抖动中不逐条发出现、提醒和恢复）。调用方持有 e.mu。
func (e *Engine) flapActive(key string) bool {
	f := e.flaps[key]
	return f != nil && f.active
}
