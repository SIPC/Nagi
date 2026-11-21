// Package alerts 在节点本地判断告警规则并直接发送，面板挂了告警照常能发出。
// 直接发送失败时（例如国内节点访问不了 Telegram）请面板代发；面板也不可达时先存在本地，之后补发。
package alerts

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
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
	// ContainerRules 返回容器生效的告警规则（面板随容器配置下发）：新版本面板给 eff，旧版本只给 legacy；
	// 都没有下发时 ok 为 false，按告警配置里的规则判断。RouteRules 同样返回 http 映射所属容器的规则。
	ContainerRules func(projectID, container string) (eff *nagiv1.EffectiveAlertRules, legacy *nagiv1.AlertRules, ok bool)
	RouteRules     func(projectID, route string) (eff *nagiv1.EffectiveAlertRules, legacy *nagiv1.AlertRules, ok bool)
	// Probes 返回 agent 报告的状态类事实（mesh 成员、见证人、日志库、节点证书、快照、副本，见 sources.go）。
	Probes Probes

	mu           sync.Mutex
	configs      map[string]*nagiv1.AlertConfig // 面板指纹 → 配置
	states       map[string]*state
	tracks       map[string]*track
	flaps        map[string]*flapState
	notices      []*nagiv1.Alert
	load         loadState
	queue        []queued
	source       string
	revision     uint64
	history      []*nagiv1.Alert
	stateErr     error             // fail closed: never publish an uncommitted source version
	committed    *nagiv1.AlertSync // newest committed version; the only one Snapshot publishes
	pending      *pendingSave      // newest version waiting for a congested store
	saving       bool              // retrySaves is running and is the only writer
	congestedLog time.Time
	suppressed   map[string]time.Time // occurrence -> bounded primary-authorized mute
	// mutedKeys：静默、维护按告警键记下，截止之前同一个键新一轮的出现也不通知（见 ApplyHandling）。
	mutedKeys map[string]time.Time
	// egressSent：出站拦截通知最近一次发送的时间（按 egress 键），用来按规则的提醒间隔去重。
	egressSent map[string]time.Time
	// counts：次数类规则的发生时间（见 Count）。
	counts map[string]*counted
	// clock 只给测试用：推进状态机的时间（为 nil 时用 time.Now）。
	clock func() time.Time
}

func (e *Engine) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
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

// New 创建告警引擎，读取本地保存的配置和待补发队列。
func New(log *slog.Logger, node, dir, certDir string, m metricSource, dc *docker.Client, projectName func(string) string, relay Relay, logs *logstore.Store) *Engine {
	e := &Engine{
		log: log, logs: logs, node: node, dir: dir, certDir: certDir, metrics: m, docker: dc,
		projectName: projectName, relay: relay,
		configs: map[string]*nagiv1.AlertConfig{}, states: map[string]*state{}, tracks: map[string]*track{}, flaps: map[string]*flapState{},
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

// panelOrder 返回面板指纹，按指纹排序（结果固定）。调用方持有 e.mu。
func (e *Engine) panelOrder() []string {
	panels := make([]string, 0, len(e.configs))
	for p := range e.configs {
		panels = append(panels, p)
	}
	sort.Strings(panels)
	return panels
}

// language 是通知语言。
func (e *Engine) language() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodeRules().lang
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
	_, set := e.rulesFor(a.Key)
	if !alert.NoticeOn(e.nodeRules().off, a.Key) {
		e.mu.Unlock()
		return // 这项事件通知被关掉了（见 alert.Notices）
	}
	send := e.setFiring(a.Key, a, set, time.Now())
	e.saveStates()
	e.mu.Unlock()
	if send != nil {
		e.dispatch(ctx, send)
	}
}

// Notify 发一条一次性的通知（例如"已顶替项目"），不跟踪状态，也没有"已恢复"。
func (e *Engine) Notify(ctx context.Context, title, message, projectID string) {
	e.mu.Lock()
	node, on := e.nodeName(), alert.NoticeOn(e.nodeRules().off, "notice")
	e.mu.Unlock()
	if !on {
		return
	}
	a := &nagiv1.Alert{Key: "notice", Status: "info", Title: title, Message: message, Node: node, Time: timestamppb.Now(), Language: e.language()}
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
	a := &nagiv1.Alert{Key: key, Status: "info", Title: title, Message: message, Node: node, Time: timestamppb.Now(), Language: e.language()}
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
		send = e.end("event:"+key, "recovered")
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

// dispatch 发到所有渠道。直接发送失败的，请配置它的面板代发；面板也不可达时放进队列。
// 发送时不持有 e.mu，避免网络慢的时候卡住配置下发。
func (e *Engine) dispatch(ctx context.Context, a *nagiv1.Alert) {
	e.mu.Lock()
	switch a.GetStatus() {
	case "info", "ended":
	case "flapping":
		if time.Now().Before(e.mutedKeys[a.GetKey()]) {
			a = nil
		}
	default:
		// 发送的是状态里的最新内容；重复提醒按调用方给的标记。
		reminder := a.GetReminder()
		if a = e.notificationAlert(queued{Source: a.GetSource(), Occurrence: a.GetOccurrence(), Key: a.GetKey(), Status: a.GetStatus()}); a != nil {
			a = proto.CloneOf(a)
			a.Reminder = reminder && a.GetStatus() == "firing"
		}
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
		if s := a.GetStatus(); !legacy && (s == "firing" || s == "resolved") {
			reminder := a.GetReminder()
			if a = e.notificationAlert(queued{Source: a.GetSource(), Occurrence: a.GetOccurrence(), Key: a.GetKey(), Status: a.GetStatus()}); a != nil {
				a = proto.CloneOf(a)
				a.Reminder = reminder && s == "firing"
			}
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
