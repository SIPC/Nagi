package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type receiver struct {
	mu   sync.Mutex
	got  []map[string]any
	fail bool
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	body, _ := io.ReadAll(req.Body)
	m := map[string]any{}
	_ = json.Unmarshal(body, &m)
	r.got = append(r.got, m)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newEngine(t *testing.T, relay Relay) *Engine {
	alert.AllowLoopbackForTest(t) // 收告警的 httptest 服务在本机
	logs, err := logstore.Open(context.Background(), filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), "node1", t.TempDir(), t.TempDir(), nil, nil,
		func(id string) string { return "name-" + id }, relay, logs)
}

func TestFireResolveAndDedupe(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e := newEngine(t, nil)
	ch := &nagiv1.AlertChannel{Id: "a", Type: "webhook", Url: srv.URL}
	// 两个面板配置了同一个 webhook，只应该发一次。
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{ch}})
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "b", Type: "webhook", Url: srv.URL}}})

	ctx := context.Background()
	e.Fire(ctx, "snapshot:x", "定时快照失败", "boom", "x")
	e.Fire(ctx, "snapshot:x", "定时快照失败", "boom", "x") // 4 小时内不重复提醒
	if n := rcv.count(); n != 1 {
		t.Fatalf("firing 发送了 %d 次，应该是 1 次", n)
	}
	if rcv.got[0]["project"] != "name-x" || rcv.got[0]["status"] != "firing" {
		t.Fatalf("内容不对：%v", rcv.got[0])
	}
	e.Resolve(ctx, "snapshot:x")
	e.Resolve(ctx, "snapshot:x")
	if n := rcv.count(); n != 2 || rcv.got[1]["status"] != "resolved" {
		t.Fatalf("恢复通知不对：%v", rcv.got)
	}

	// 配置保存在本地，重启后还在。
	e2 := newEngine(t, nil)
	e2.dir = e.dir
	e2.loadConfigs()
	if len(e2.channels()) != 1 {
		t.Fatalf("重新读取配置后渠道数 %d", len(e2.channels()))
	}
}

func TestHandlingSurvivesRestartWithoutSuppressingRecoveryOrNewOccurrence(t *testing.T) {
	e := newEngine(t, nil)
	ctx := t.Context()
	e.Fire(ctx, "backup:p/web", "backup failed", "", "p")
	snapshot := e.Snapshot()
	if snapshot == nil || len(snapshot.Current) != 1 {
		t.Fatal("channel-free detection missing")
	}
	e.ApplyHandling([]*nagiv1.AlertState{{Alert: snapshot.Current[0], SilenceUntil: timestamppb.New(time.Now().Add(time.Hour))}})
	e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e.SetConfig("p", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}})
	e.Fire(ctx, "backup:p/web", "still failed", "", "p")
	if rcv.count() != 0 {
		t.Fatal("restart lost silence")
	}
	e.Resolve(ctx, "backup:p/web")
	terminal := e.Snapshot()
	if len(terminal.History) != 1 || terminal.History[0].Revision == 0 || rcv.count() != 1 {
		t.Fatalf("missing terminal version or recovery: %v", terminal)
	}
	// 静默按告警键记下（见 docs/monitoring.md#告警）：截止之前同一个键新一轮的出现也不通知，但照样记录。
	e.Fire(ctx, "backup:p/web", "new failure", "", "p")
	if rcv.count() != 1 || e.Snapshot().Current[0].GetOccurrence() == terminal.History[0].GetOccurrence() {
		t.Fatal("silence must cover a new occurrence of the same key")
	}
	if _, err := os.Stat(e.stateFile()); !os.IsNotExist(err) {
		t.Fatalf("new state leaked outside logs.db: %v", err)
	}
	if err := e.logs.Close(); err != nil {
		t.Fatal(err)
	}
	e.Fire(ctx, "backup:p/web", "failed", "", "p")
	if e.Snapshot() != nil {
		t.Fatal("failed store published uncommitted state")
	}
}

// v0.37.0 paused notifications and sync for as long as the shared log store was
// congested. Now detection and delivery continue; only the published snapshot
// waits until the newer version is committed.
func TestCongestedStoreDoesNotPauseNotifications(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e := newEngine(t, nil)
	ctx := t.Context()
	e.SetConfig("p", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}})
	before := e.Snapshot()
	held, release := make(chan struct{}), make(chan struct{})
	go e.logs.WriteArchive(context.Background(), 4, 0, func() error { close(held); <-release; return nil })
	<-held
	var once sync.Once
	unhold := func() { once.Do(func() { close(release) }) }
	defer unhold()
	fired := make(chan struct{})
	go func() { e.Fire(ctx, "backup:p/web", "backup failed", "", "p"); close(fired) }()
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("alert evaluation waited for the congested store")
	}
	if rcv.count() != 1 {
		t.Fatalf("notification paused by congested store: %v", rcv.got)
	}
	if s := e.Snapshot(); s == nil || s.Revision != before.Revision || len(s.Current) != 0 {
		t.Fatalf("published an uncommitted version: %v", s)
	}
	unhold()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s := e.Snapshot(); s != nil && len(s.Current) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background save never committed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLegacyCheckpointImportsOnceWithoutDeletingOriginal(t *testing.T) {
	e := newEngine(t, nil)
	ctx := t.Context()
	e.Fire(ctx, "backup:p/web", "backup failed", "", "p")
	before := e.Snapshot()
	legacy, err := e.logs.Checkpoint(ctx, "alerts/state/agent")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "alerts-state.json")
	if err := os.WriteFile(file, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	restart := func() *Engine { return New(e.log, e.node, dir, e.certDir, nil, nil, e.projectName, nil, logs) }
	migrated := restart()
	after := migrated.Snapshot()
	if after == nil || after.Source != before.Source || len(after.Current) != 1 || after.Current[0].Occurrence != before.Current[0].Occurrence {
		t.Fatalf("legacy state not preserved: %v", after)
	}
	migrated.Resolve(ctx, "backup:p/web")
	after = restart().Snapshot()
	if after == nil || len(after.Current) != 0 || len(after.History) != 1 || after.History[0].Status != "resolved" {
		t.Fatalf("restart reimported obsolete legacy state: %v", after)
	}
	retained, err := os.ReadFile(file)
	if err != nil || string(retained) != string(legacy) {
		t.Fatalf("legacy file was modified or removed: %v", err)
	}
}

func TestRelayAndQueue(t *testing.T) {
	rcv := &receiver{fail: true}
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	var relayed []string
	relayOK := false
	e := newEngine(t, func(_ context.Context, panel string, a *nagiv1.Alert, ids []string) error {
		if !relayOK {
			return errors.New("面板不可达")
		}
		relayed = append(relayed, panel+"/"+ids[0]+"/"+a.GetStatus())
		return nil
	})
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "a", Type: "webhook", Url: srv.URL}}})
	ctx := context.Background()

	e.Fire(ctx, "k", "t", "", "")
	if len(e.queue) != 1 {
		t.Fatalf("直接发送和代发都失败时应该进队列，队列长度 %d", len(e.queue))
	}
	// 面板恢复后，补发走代发。
	relayOK = true
	e.flushQueue(ctx)
	if len(e.queue) != 0 || len(relayed) != 1 || relayed[0] != "p1/a/firing" {
		t.Fatalf("补发不对：queue=%d relayed=%v", len(e.queue), relayed)
	}
	// 渠道恢复后直接发送。
	rcv.mu.Lock()
	rcv.fail = false
	rcv.mu.Unlock()
	e.Resolve(ctx, "k")
	if rcv.count() != 1 || len(relayed) != 1 {
		t.Fatalf("直接发送不对：direct=%d relayed=%v", rcv.count(), relayed)
	}
}

// 告警里的节点名称用面板下发的（节点可能在面板上改过名），没有时用主机名。
func TestNodeName(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	ch := []*nagiv1.AlertChannel{{Id: "a", Type: "webhook", Url: srv.URL}}
	ctx := context.Background()

	e := newEngine(t, nil)
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: ch})
	e.Fire(ctx, "k1", "t", "", "")
	if got := rcv.got[0]["node"]; got != "node1" {
		t.Fatalf("面板没下发名称时应该用主机名，得到 %v", got)
	}

	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Channels: ch, NodeName: "东京-1"})
	e.Resolve(ctx, "k1")
	e.Notify(ctx, "t", "", "")
	for _, m := range rcv.got[1:] {
		if m["node"] != "东京-1" {
			t.Fatalf("应该用面板下发的名称：%v", m)
		}
	}

	// 两个面板的名称不同时（移交过程中），按面板指纹取第一个；重启后从本地配置读回。
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{NodeName: "大阪-1"})
	e2 := newEngine(t, nil)
	e2.dir = e.dir
	e2.loadConfigs()
	if got := e2.nodeName(); got != "大阪-1" {
		t.Fatalf("重新读取配置后名称是 %q", got)
	}
}

// 几个面板管同一个节点时取最严格的：阈值更低、恢复更难、持续更短；任何一个开着就判断。
func TestStrictestRules(t *testing.T) {
	e := newEngine(t, nil)
	p1 := alert.Effective(&nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleDisk: {Threshold: f64(80), RecoverThreshold: f64(70)}}}, true)
	p2 := alert.Effective(&nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{alert.RuleDisk: {Threshold: f64(95)}, alert.RuleCert: {Threshold: f64(14), RecoverThreshold: f64(20)},
		alert.RuleMem: {Enabled: on(false)}}}, true)
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Effective: p1})
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Effective: p2})
	// 旧版本面板只有 AlertRules：按换算后的生效值参与合并。
	_ = e.SetConfig("p3", &nagiv1.AlertConfig{Rules: &nagiv1.AlertRules{MemPercent: 70}})
	e.mu.Lock()
	r := e.nodeRules()
	e.mu.Unlock()
	if d := r.get(alert.RuleDisk); d.GetThreshold() != 80 || d.GetRecoverThreshold() != 70 {
		t.Fatalf("磁盘应取 80/70：%v", d)
	}
	if c := r.get(alert.RuleCert); c.GetThreshold() != 14 || c.GetRecoverThreshold() != 20 {
		t.Fatalf("证书是越低越糟的规则，应取 14/20：%v", c)
	}
	if m := r.get(alert.RuleMem); !alert.On(m) || m.GetThreshold() != 70 {
		t.Fatalf("p1、p3 开着内存告警，阈值取旧版本面板的 70：%v", m)
	}
}

// 容器的规则按面板随容器配置下发的判断；没有下发（旧版本面板）时用告警配置里的。
func TestRuleSwitches(t *testing.T) {
	e := newEngine(t, nil)
	_ = e.SetConfig("p1", &nagiv1.AlertConfig{Rules: &nagiv1.AlertRules{Enabled: map[string]bool{"snapshot": false}}})
	_ = e.SetConfig("p2", &nagiv1.AlertConfig{Rules: &nagiv1.AlertRules{Enabled: map[string]bool{"snapshot": false}}})
	e.ContainerRules = func(project, container string) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules, bool) {
		switch container {
		case "web": // 新版本面板
			return alert.Effective(nil, false, map[string]*nagiv1.AlertRule{alert.RuleBackup: {Enabled: on(false)}, alert.RuleRestart: {Threshold: f64(5)}}), nil, true
		case "v37": // v0.37 面板只下发旧格式
			return nil, &nagiv1.AlertRules{RestartCount: 7}, true
		}
		return nil, nil, false
	}
	if e.ContainerEnabled("p", "web", "backup") || !e.ContainerEnabled("p", "web", "snapshot") {
		t.Error("web 按面板下发的规则：备份关、快照开")
	}
	if e.ContainerEnabled("p", "old", "snapshot") {
		t.Error("没有下发规则的容器按告警配置：两个面板都关了快照告警")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// 送达前复核（补发队列）：备份、热备告警按容器的规则判断（以前按节点规则，节点规则里没有 backup，永远放行）。
	if e.ruleEnabled("event:backup:p/web") || e.ruleEnabled("event:standby:p/web") || !e.ruleEnabled("event:snapshot:p/web") {
		t.Error("复核应按 web 的规则：备份、热备关，快照开")
	}
	if n := e.forContainer("p", "web").get(alert.RuleRestart).GetThreshold(); n != 5 {
		t.Errorf("web 的重启次数 %v，应该是 5", n)
	}
	if n := e.forContainer("p", "v37").get(alert.RuleRestart).GetThreshold(); n != 7 {
		t.Errorf("v37 按旧格式换算，应该是 7，得到 %v", n)
	}
	if n := e.forContainer("p", "old").get(alert.RuleRestart).GetThreshold(); n != 3 {
		t.Errorf("old 用内置默认值 3，得到 %v", n)
	}
}

func f64(v float64) *float64 { return &v }
func u32(v uint32) *uint32   { return &v }
func on(v bool) *bool        { return &v }

type observations struct {
	latest     map[string]float64
	data       map[string][]metrics.Point
	queryErr   error
	containers []docker.Instance
	dockerErr  error
}

func (o *observations) LatestFresh(time.Time) map[string]float64 { return o.latest }
func (o *observations) Query(context.Context, string, time.Time, time.Time) (map[string][]metrics.Point, error) {
	return o.data, o.queryErr
}
func (o *observations) ManagedInstances(context.Context, string) ([]docker.Instance, error) {
	return o.containers, o.dockerErr
}

func routeSamples(at time.Time, requests, client, server float64) map[string][]metrics.Point {
	at = at.Truncate(metrics.Interval)
	out := map[string][]metrics.Point{}
	for kind, value := range map[string]float64{"req": requests, "4xx": client, "5xx": server} {
		for i := 0; i < 30; i++ {
			key := "r:p/web:" + kind
			out[key] = append(out[key], metrics.Point{Time: at.Add(time.Duration(i-29) * metrics.Interval), Value: value})
		}
	}
	return out
}

func TestRecoveryRequiresPositiveEvidence(t *testing.T) {
	for _, failure := range []string{"query failure", "missing counters", "stale window", "time mismatch", "low sample", "gap", "disabled", "empty docker list", "starting"} {
		t.Run(failure, func(t *testing.T) {
			rcv := &receiver{}
			srv := httptest.NewServer(rcv)
			defer srv.Close()
			// 这个测试只看「有没有健康证据」，持续时间都设为 0。
			disabled := false
			rules := func() *nagiv1.AlertPolicy {
				p := &nagiv1.AlertPolicy{Rules: map[string]*nagiv1.AlertRule{}}
				for _, r := range []string{alert.RuleDisk, alert.RuleHealth, alert.RuleClientErrors, alert.RuleErrors} {
					p.Rules[r] = &nagiv1.AlertRule{ForSeconds: u32(0), RecoverSeconds: u32(0), Enabled: on(!disabled)}
				}
				return p
			}
			ch := []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}
			var e *Engine
			setup := func(x *Engine) {
				e = x
				e.SetConfig("p", &nagiv1.AlertConfig{Channels: ch, Effective: alert.Effective(rules(), true)})
				e.ContainerRules = func(string, string) (*nagiv1.EffectiveAlertRules, *nagiv1.AlertRules, bool) {
					return alert.Effective(rules(), false), nil, true
				}
				e.RouteRules = e.ContainerRules
			}
			setup(newEngine(t, nil))
			o := &observations{latest: map[string]float64{"node:disk": 95}, data: routeSamples(time.Now(), 1, .6, .2),
				containers: []docker.Instance{{Project: "p", Container: "web", Service: "app", State: "running", Health: "unhealthy"}}}
			e.metrics, e.docker = o, o
			e.evaluate(context.Background())
			if rcv.count() != 4 {
				t.Fatalf("expected disk, health, 4xx and 5xx firing, got %v", rcv.got)
			}

			// No fresh disk evidence. The container is missing/unknown, not healthy.
			o.latest, o.containers = nil, nil
			o.dockerErr = errors.New("Docker unavailable")
			o.data = nil
			switch failure {
			case "query failure":
				o.queryErr = errors.New("database unavailable")
			case "missing counters":
				o.data = routeSamples(time.Now(), 1, 0, 0)
				delete(o.data, "r:p/web:4xx")
				delete(o.data, "r:p/web:5xx")
			case "stale window":
				o.data = routeSamples(time.Now().Add(-time.Minute), 1, 0, 0)
			case "time mismatch":
				o.data = routeSamples(time.Now(), 1, 0, 0)
				o.data["r:p/web:req"][10].Time = o.data["r:p/web:req"][10].Time.Add(time.Second)
			case "low sample":
				o.data = routeSamples(time.Now(), .01, 0, 0)
			case "gap":
				o.data = routeSamples(time.Now(), 1, 0, 0)
				for key, points := range o.data {
					o.data[key] = append(points[:5], points[15:]...)
				}
			case "disabled":
				o.latest, o.data, o.dockerErr = map[string]float64{"node:disk": 5}, routeSamples(time.Now(), 1, 0, 0), nil
				o.containers = []docker.Instance{{Project: "p", Container: "web", Service: "app", State: "running", Health: "healthy"}}
				disabled = true
				setup(e)
			case "empty docker list":
				o.dockerErr = nil
			case "starting":
				o.dockerErr = nil
				o.containers = []docker.Instance{{Project: "p", Container: "web", Service: "app", State: "running", Health: "starting"}}
			}
			e.evaluate(context.Background())
			if rcv.count() != 4 {
				t.Fatalf("%s sent a false recovery: %v", failure, rcv.got)
			}
			setup(New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs))
			e.metrics, e.docker = o, o
			e.evaluate(context.Background())
			if rcv.count() != 4 {
				t.Fatalf("restart invented a new failure or recovery: %v", rcv.got)
			}

			// Restore the rules and provide actual fresh healthy evidence.
			disabled = false
			setup(e)
			o.latest, o.data, o.queryErr, o.dockerErr = map[string]float64{"node:disk": 10}, routeSamples(time.Now(), 1, 0, 0), nil, nil
			o.containers = []docker.Instance{{Project: "p", Container: "web", Service: "app", State: "running", Health: "healthy"}}
			e.evaluate(context.Background())
			if failure == "disabled" {
				// 关掉规则时这一轮已经以 rule_disabled 结束（不是恢复，不通知），重新打开后没有可恢复的告警。
				for _, a := range e.Snapshot().History {
					if a.GetEndReason() != "rule_disabled" {
						t.Fatalf("expected rule_disabled endings: %v", a)
					}
				}
				if rcv.count() != 4 {
					t.Fatalf("disabling a rule must not notify: %v", rcv.got)
				}
				return
			}
			if rcv.count() != 8 {
				t.Fatalf("expected four genuine recoveries: %v", rcv.got)
			}
			for _, a := range rcv.got[4:] {
				if a["status"] != "resolved" {
					t.Fatalf("unexpected recovery: %v", a)
				}
			}
		})
	}
}

func TestOccurrenceSurvivesRestartAndRemovalDoesNotResolve(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e := newEngine(t, nil)
	e.SetConfig("p", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}})
	ctx := context.Background()
	e.Fire(ctx, "cert_failed:example.com", "certificate failed", "", "")
	e.Fire(ctx, "cert_failed:example.com", "certificate still failed", "", "")
	before := e.states["event:cert_failed:example.com"].occurrence

	restart := func() *Engine {
		return New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
	}
	e = restart()
	e.Fire(ctx, "cert_failed:example.com", "certificate still failed", "", "")
	if rcv.count() != 1 || e.states["event:cert_failed:example.com"].occurrence != before {
		t.Fatal("restart created a duplicate first occurrence")
	}
	e.MarkUnavailableUnless("cert_failed:", func(string) bool { return false })
	e = restart()
	if rcv.count() != 1 || e.states["event:cert_failed:example.com"].confirmed {
		t.Fatal("object removal or restart invented recovery evidence")
	}
	e.Resolve(ctx, "cert_failed:example.com")
	e = restart()
	e.Resolve(ctx, "cert_failed:example.com")
	if rcv.count() != 2 || rcv.got[1]["status"] != "resolved" {
		t.Fatalf("recovery was lost or duplicated: %v", rcv.got)
	}
	e.Fire(ctx, "cert_failed:example.com", "new failure", "", "")
	if rcv.count() != 3 || e.states["event:cert_failed:example.com"].occurrence == before {
		t.Fatal("a new failure must have a new occurrence")
	}
	if data, err := e.logs.Checkpoint(ctx, "alerts/state/agent"); err != nil || !json.Valid(data) {
		t.Fatalf("state checkpoint invalid: %s %v", data, err)
	}
}

func TestNoChannelDoesNotCheckpointDelivery(t *testing.T) {
	e := newEngine(t, nil)
	ctx := context.Background()
	e.Fire(ctx, "backup:p/web", "backup failed", "", "")
	e = New(e.log, e.node, e.dir, e.certDir, nil, nil, e.projectName, nil, e.logs)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	e.SetConfig("p", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}})
	e.Fire(ctx, "backup:p/web", "backup still failed", "", "")
	if rcv.count() != 1 {
		t.Fatal("checkpoint treated an unsent alert as already accepted")
	}
}
