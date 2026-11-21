package alerts

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// harness 每 30 秒（和节点一样）判断一次，时间由测试推进。
type harness struct {
	t      *testing.T
	e      *Engine
	rcv    *receiver
	o      *observations
	now    time.Time
	ch     []*nagiv1.AlertChannel
	routes func(now time.Time) map[string][]metrics.Point
}

func newHarness(t *testing.T, policy *nagiv1.AlertPolicy) *harness {
	h := &harness{t: t, rcv: &receiver{}, o: &observations{}, now: time.Now()}
	srv := httptest.NewServer(h.rcv)
	t.Cleanup(srv.Close)
	h.e = newEngine(t, nil)
	h.e.clock = func() time.Time { return h.now }
	h.e.metrics, h.e.docker = h.o, h.o
	h.ch = []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}
	h.policy(policy)
	return h
}

func (h *harness) policy(p *nagiv1.AlertPolicy) {
	_ = h.e.SetConfig("p", &nagiv1.AlertConfig{Channels: h.ch, Effective: alert.Effective(p, true)})
	h.e.ContainerRules = func(string, string) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules, bool) {
		return alert.Effective(p, false), nil, true
	}
	h.e.RouteRules = h.e.ContainerRules
}

// feed 用同一组最新值判断 n 次（每次 30 秒）。latest 为 nil 表示没有数据。
func (h *harness) feed(latest map[string]float64, n int) {
	for range n {
		h.now = h.now.Add(30 * time.Second)
		h.o.latest = latest
		if h.routes != nil {
			h.o.data = h.routes(h.now)
		}
		h.e.evaluate(context.Background())
	}
}

func (h *harness) sent() int { return h.rcv.count() }

func (h *harness) last() map[string]any {
	h.rcv.mu.Lock()
	defer h.rcv.mu.Unlock()
	return h.rcv.got[len(h.rcv.got)-1]
}

func (h *harness) expect(n int, why string) {
	h.t.Helper()
	if got := h.sent(); got != n {
		h.rcv.mu.Lock()
		defer h.rcv.mu.Unlock()
		h.t.Fatalf("%s：应该一共发了 %d 条，实际 %d 条：%v", why, n, got, h.rcv.got)
	}
}

func (h *harness) expectText(prefix string, contains ...string) {
	h.t.Helper()
	text, _ := h.last()["text"].(string)
	if !strings.HasPrefix(text, prefix) {
		h.t.Fatalf("文字应该以 %q 开头：%q", prefix, text)
	}
	for _, c := range contains {
		if !strings.Contains(text, c) {
			h.t.Fatalf("文字里应该有 %q：%q", c, text)
		}
	}
}

func mem(v float64) map[string]float64 { return map[string]float64{"node:mem": v} }

// 截图里的情况：内存在 90% 上下波动。现在要超过 90% 持续 5 分钟才告警，回到 85% 以下并保持 5 分钟才恢复；
// 恢复通知写恢复时的值，不再照搬最后一次超标的标题。
func TestMemoryHysteresisAndRecoveryText(t *testing.T) {
	h := newHarness(t, nil)
	h.feed(mem(92), 10) // 4 分 30 秒
	h.expect(0, "超标不满 5 分钟")
	h.feed(mem(92), 1)
	h.expect(1, "超标满 5 分钟")
	h.expectText("🔴 内存使用 92%，超过 90% 已 5 分钟", "节点：node1")
	if v := h.last()["threshold"]; v != 90.0 {
		t.Fatalf("webhook 应带阈值：%v", h.last())
	}

	h.feed(mem(93), 2)
	h.feed(mem(89.9), 2) // 截图里在这里发了「已恢复」
	h.feed(mem(86), 4)
	h.expect(1, "还在回差区间（> 85%）里，不算恢复")

	h.feed(mem(84), 6)
	h.feed(mem(87), 1) // 恢复中又超过恢复阈值：回到告警中，不发通知，重新计时
	h.feed(mem(84), 10)
	h.expect(1, "回到 85% 以下还不满 5 分钟")
	h.feed(mem(84), 1)
	h.expect(2, "保持 5 分钟后恢复")
	h.expectText("✅ 已恢复：内存使用降到 84%（恢复阈值 85%，已稳定 5 分钟）", "本次持续 ", "最高 93%")
	if got := h.last(); got["status"] != "resolved" || got["value"] != 84.0 || got["peak"] != 93.0 {
		t.Fatalf("恢复通知的结构化字段不对：%v", got)
	}
	// 待触发期间回落：不发任何通知。
	h.feed(mem(95), 3)
	h.feed(mem(50), 1)
	h.feed(mem(95), 9)
	h.expect(2, "待触发期间回落要重新计时")
}

// 缺数据不判定恢复，短暂缺数据也不清零恢复计时。
func TestMissingDataNeitherRecoversNorResets(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {ForSeconds: u32(0)}}})
	h.feed(mem(95), 1)
	h.expect(1, "持续时间为 0 时第一次超标就告警")
	h.feed(mem(80), 5) // 进入恢复中 2 分钟
	h.feed(nil, 2)     // 缺 1 分钟数据（不到 90 秒）
	h.expect(1, "缺数据不是恢复")
	h.feed(mem(80), 3)
	h.expect(1, "恢复计时还差 30 秒")
	h.feed(mem(80), 1)
	h.expect(2, "缺数据前后的恢复时间连起来满 5 分钟")
}

// 告警中按规则的间隔重复提醒，之前不重复通知。
func TestRepeatReminder(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {ForSeconds: u32(0), RepeatMinutes: u32(60)}}})
	h.feed(mem(95), 1)
	h.feed(mem(88), 60) // 回差区间里也是告警中
	h.feed(mem(96), 59)
	h.expect(1, "不到 60 分钟不重复提醒")
	h.feed(mem(96), 1)
	h.expect(2, "满 60 分钟提醒一次")
	h.expectText("🔁 仍未恢复：内存使用 96%，超过 90% 已 1 小时")
	if h.last()["reminder"] != true {
		t.Fatalf("webhook 应标明是重复提醒：%v", h.last())
	}
}

// 60 分钟内触发 3 轮进入抖动：发一条「反复出现」，之后不逐条通知；保持告警 30 分钟后退出抖动并补发出现通知。
func TestFlapProtection(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{
		alert.RuleDisk: {ForSeconds: u32(0), RecoverSeconds: u32(0), RepeatMinutes: u32(0)}}})
	disk := func(v float64) map[string]float64 { return map[string]float64{"node:disk": v} }
	for range 2 {
		h.feed(disk(95), 1)
		h.feed(disk(50), 1)
	}
	h.expect(4, "前两轮照常通知")
	h.feed(disk(95), 1)
	h.expect(5, "第三轮只发一条「反复出现」")
	if h.last()["status"] != "flapping" {
		t.Fatalf("应该是反复出现：%v", h.last())
	}
	h.expectText("🟡 反复出现：60 分钟内触发了 3 次")
	h.feed(disk(50), 1)
	h.feed(disk(95), 1)
	h.feed(disk(95), 59)
	h.expect(5, "抖动中不逐条通知")
	h.feed(disk(95), 1)
	h.expect(6, "保持告警 30 分钟后退出抖动，补发出现通知")
	if h.last()["status"] != "firing" {
		t.Fatalf("应该补发出现通知：%v", h.last())
	}
	h.feed(disk(50), 1)
	h.expect(7, "退出抖动后照常发恢复")
}

// 静默按告警键记下：恢复照常通知，静默期内同一个键新一轮的出现不通知，但照样记录。
func TestSilenceCoversNewOccurrenceOfSameKey(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {ForSeconds: u32(0), RecoverSeconds: u32(0)}}})
	h.feed(mem(95), 1)
	cur := h.e.Snapshot().Current[0]
	h.e.ApplyHandling([]*nagiv1.AlertState{{Alert: cur, SilenceUntil: timestamppb.New(time.Now().Add(time.Hour))}})
	h.feed(mem(50), 1)
	h.expect(2, "静默不压恢复通知")
	h.feed(mem(95), 1)
	h.expect(2, "静默期内同一个键新一轮不通知")
	if s := h.e.Snapshot(); len(s.Current) != 1 || s.Current[0].GetOccurrence() == cur.GetOccurrence() {
		t.Fatalf("新一轮应该照样记录：%v", s.Current)
	}
}

// 告警中关掉规则：以 rule_disabled 结束（不是恢复），不通知，也不再挂在当前告警里。
func TestDisablingRuleEndsAlert(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {ForSeconds: u32(0)}}})
	h.feed(mem(95), 1)
	h.policy(&nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {Enabled: on(false)}}})
	h.feed(mem(95), 2)
	h.expect(1, "关掉规则不发通知")
	s := h.e.Snapshot()
	if len(s.Current) != 0 || len(s.History) != 1 || s.History[0].GetEndReason() != "rule_disabled" || s.History[0].GetStatus() != "ended" {
		t.Fatalf("应该以 rule_disabled 结束：%v", s)
	}
}

// 4xx 比例：请求数够时按阈值判断；告警中请求一直不够 30 分钟，以「数据不足」结束，不发「已恢复」。
func TestRouteLowTrafficEndsAsInsufficientData(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleClientErrors: {MinRequests: f64(200)}}})
	rate, share := 1.0, 0.6
	h.routes = func(now time.Time) map[string][]metrics.Point { return routeSamples(now, rate, rate*share, 0) }
	h.feed(nil, 1)
	h.expect(1, "300 个请求里 60% 是 4xx")
	h.expectText("🔴 映射 web 的 4xx 比例 60%，超过 50%", "最近 5 分钟共 300 个请求")
	rate = 0.1 // 30 个请求，不够 200
	h.feed(nil, 60)
	h.expect(1, "请求不够时不判断")
	h.feed(nil, 1)
	h.expect(2, "30 分钟请求都不够")
	if got := h.last(); got["status"] != "ended" || got["end_reason"] != "insufficient_data" {
		t.Fatalf("应该以数据不足结束：%v", got)
	}
	h.expectText("⚪ 已结束：", "这不是恢复")
}

// ranged 按查询的时间范围返回样本（基线和统计窗口查的范围不同）。
type ranged struct{ *observations }

func (r ranged) Query(_ context.Context, prefix string, from, to time.Time) (map[string][]metrics.Point, error) {
	out := map[string][]metrics.Point{}
	for k, pts := range r.data {
		for _, p := range pts {
			if strings.HasPrefix(k, prefix) && !p.Time.Before(from) && !p.Time.After(to) {
				out[k] = append(out[k], p)
			}
		}
	}
	return out, nil
}

// l4Samples 是映射 p/tcp 最近 span 的样本：新连接/秒、失败/秒、入出字节/秒，每 10 秒一个。
func l4Samples(now time.Time, span time.Duration, conn, fail, in, out float64) map[string][]metrics.Point {
	now = now.Truncate(metrics.Interval)
	data := map[string][]metrics.Point{}
	for kind, v := range map[string]float64{"conn": conn, "fail": fail, "in": in, "out": out, "active": conn * 10} {
		for t := now.Add(-span + metrics.Interval); !t.After(now); t = t.Add(metrics.Interval) {
			data["r:p/tcp:"+kind] = append(data["r:p/tcp:"+kind], metrics.Point{Time: t, Value: v})
		}
	}
	return data
}

// 新连接突增：基线不足 6 小时时不判断（再高也不告警），基线够了以后超过基线 10 倍且每秒超过 20 个才告警；
// 每秒没到下限的高倍数不算。
func TestL4SurgeWaitsForBaseline(t *testing.T) {
	h := newHarness(t, nil)
	h.e.metrics = ranged{h.o}
	conn := 1.0
	history := 5 * time.Hour
	h.routes = func(now time.Time) map[string][]metrics.Point {
		// 历史样本固定 1/秒，最近 5 分钟是 conn。
		d := l4Samples(now.Add(-alert.ErrorWindowDefault), history, 1, 0, 0, 0)
		for k, v := range l4Samples(now, alert.ErrorWindowDefault, conn, 0, 0, 0) {
			d[k] = append(d[k], v...)
		}
		return d
	}
	conn = 50
	h.feed(nil, 20)
	h.expect(0, "基线只有 5 小时，不判断突增")
	if b := h.e.Baselines(); len(b) != 1 || b[0].GetStatus() != "building" || b[0].GetRequiredSeconds() != 6*3600 {
		t.Fatalf("应该在建立中：%v", b)
	}

	history = 7 * time.Hour
	h.e.load.baselineAt = time.Time{} // 立即重算基线
	h.feed(nil, 4) // 基线可用的第一轮开始计时，第 4 轮时 1 分 30 秒
	h.expect(0, "突增要持续 2 分钟")
	h.feed(nil, 1)
	h.expect(1, "每秒 50 个，是基线的 50 倍")
	if b := h.e.Baselines(); len(b) != 1 || b[0].GetStatus() != "ready" {
		t.Fatalf("基线应该可用：%v", b)
	}
	h.expectText("🔴 映射 tcp 的新连接", "基线每秒 1 个")

	conn = 15 // 15 倍，但每秒不到 20 个：算恢复
	h.feed(nil, 10)
	h.expect(1, "低于每秒下限还不满 5 分钟")
	h.feed(nil, 1)
	h.expect(2, "低于每秒下限，持续 5 分钟后恢复")
}

// 连接失败比例：连接数不够时不判断，告警中一直不够 30 分钟就以「数据不足」结束。
func TestL4FailNeedsEnoughConnections(t *testing.T) {
	h := newHarness(t, nil)
	conn, fail := 1.0, 0.3
	h.routes = func(now time.Time) map[string][]metrics.Point {
		return l4Samples(now, alert.ErrorWindowDefault, conn, fail, 0, 0)
	}
	h.feed(nil, 1)
	h.expect(1, "300 个连接里 30% 失败")
	h.expectText("🔴", "30%", "20%")
	conn, fail = 0.05, 0.05 // 15 个连接，不够 20
	h.feed(nil, 60)
	h.expect(1, "连接不够不判断")
	h.feed(nil, 1)
	if got := h.last(); got["end_reason"] != "insufficient_data" {
		t.Fatalf("应该以数据不足结束：%v", got)
	}
}

// 带宽默认关闭；打开后入和出分开告警。
func TestL4BandwidthOffByDefaultAndPerDirection(t *testing.T) {
	h := newHarness(t, nil)
	h.routes = func(now time.Time) map[string][]metrics.Point {
		return l4Samples(now, alert.ErrorWindowDefault, 1, 0, 1e3, 20e6) // 出 160 Mbit/s
	}
	h.feed(nil, 10)
	h.expect(0, "带宽规则默认关闭")
	h.policy(&nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleL4Bandwidth: {Enabled: on(true), ForSeconds: u32(0)}}})
	h.feed(nil, 1)
	h.expect(1, "只有出方向超过 100 Mbit/s")
	h.expectText("🔴", "（出）", "160", "100")
}

// 次数类规则（内核 OOM）：窗口里发生 1 次就告警；之后连续 30 分钟没有新的才恢复，中间再发生要重新计时。
func TestCountRuleRecoversAfterQuietPeriod(t *testing.T) {
	h := newHarness(t, nil)
	h.e.Count("kernel_oom", alert.RuleKernelOOM, "", "", "", h.now)
	h.feed(nil, 1)
	h.expect(1, "发生 1 次就告警")
	h.feed(nil, 40) // 20 分钟
	h.e.Count("kernel_oom", alert.RuleKernelOOM, "", "", "", h.now)
	h.feed(nil, 61) // 第一轮还看得到这次发生，之后 30 分钟没有新的
	h.expect(1, "又发生了一次，恢复要重新计时")
	h.feed(nil, 1)
	h.expect(2, "最后一次以后安静 30 分钟")
	if h.last()["status"] != "resolved" {
		t.Fatalf("应该是恢复：%v", h.last())
	}
}

// 状态类规则：mesh 成员断开要持续阈值（300 秒）；没有证据（Bad、Good 都是 false）既不触发也不恢复。
func TestFactsHoldForThresholdAndNeedEvidence(t *testing.T) {
	h := newHarness(t, nil)
	state := "bad"
	h.e.Probes = func(context.Context, time.Time) []Fact {
		f := Fact{Key: "mesh_peer:abc", Rule: alert.RuleMeshPeer, Subject: "node-b"}
		f.Bad, f.Good = state == "bad", state == "good"
		return []Fact{f}
	}
	h.feed(nil, 10) // 第一轮开始计时，第 10 轮时断开 4 分 30 秒
	h.expect(0, "断开不满 5 分钟")
	h.feed(nil, 1)
	h.expect(1, "断开满 5 分钟")
	state = "unknown"
	h.feed(nil, 20)
	h.expect(1, "没有证据不算恢复")
	state = "good"
	h.feed(nil, 4)
	h.expect(1, "连上不满 2 分钟")
	h.feed(nil, 1)
	h.expect(2, "连上并保持 2 分钟后恢复")
}

// 关掉的事件通知（顶替失败等）不发送，其他照常。
func TestDisabledNoticeIsNotSent(t *testing.T) {
	h := newHarness(t, &nagiv1.AlertPolicy{Notifications: map[string]bool{"takeover": false}})
	h.e.Fire(context.Background(), "takeover:p/web", "顶替失败", "", "")
	h.expect(0, "顶替通知被关掉了")
	h.e.Fire(context.Background(), "failback:p/web", "切回失败", "", "")
	h.expect(1, "切回通知照常")
}

// 备份、热备同步失败按容器的 backup 规则复核（以前按节点规则复核，节点规则里没有 backup，永远放行）：
// 容器关掉 backup 以后，补发队列里的出现通知不再发送。
func TestBackupRecheckUsesContainerRule(t *testing.T) {
	h := newHarness(t, nil)
	backupOff := false
	h.e.ContainerRules = func(project, container string) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules, bool) {
		p := &nagiv1.AlertPolicy{}
		if container == "web" && backupOff {
			p.Rules = map[string]*nagiv1.AlertRule{alert.RuleBackup: {Enabled: on(false)}}
		}
		return alert.Effective(p, false), nil, true
	}
	h.e.mu.Lock()
	for _, key := range []string{"event:backup:p/web", "event:standby:p/web", "event:backup:p/db"} {
		if !h.e.ruleEnabled(key) {
			t.Fatalf("%s 默认应放行", key)
		}
	}
	backupOff = true
	if h.e.ruleEnabled("event:backup:p/web") || h.e.ruleEnabled("event:standby:p/web") {
		t.Fatal("容器关掉 backup 后，备份和热备的通知都不应放行")
	}
	if !h.e.ruleEnabled("event:backup:p/db") {
		t.Fatal("别的容器不受影响")
	}
	h.e.mu.Unlock()
}
