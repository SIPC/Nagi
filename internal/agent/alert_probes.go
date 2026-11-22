package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 节点上状态类告警的事实（见 docs/monitoring.md#告警 和 alerts/sources.go）：每轮判断时由告警引擎来取。
// 这里只报告单项事实，判断、持续时间、回差都在告警引擎里。

// alertFacts 记着几个只在事件发生时才知道的事实（证书续期、端口监听）。
type alertFacts struct {
	mu sync.Mutex
	// certFails：节点证书连续续期失败的次数（成功后清零），certExpiry 是证书到期时间。
	certFails  int
	certExpiry time.Time
	certErr    string
	// listen：端口监听失败的映射（项目 ID/映射名 → 错误），和最近一次成功加载的时间。
	listen   map[string]string
	listenOK time.Time
	// logErrors：日志库上一次的写入失败累计数（Count 记增量）。
	logErrors uint64
	logSeen   bool
	// logAfter：各类别读到的位置（节点启动时从日志库末尾开始，不把以前的事件当成新的）。
	logAfter map[string]int64
}

// alertProbes 返回这一轮的事实。
func (a *agent) alertProbes(ctx context.Context, now time.Time) []alerts.Fact {
	var out []alerts.Fact
	out = append(out, a.meshFacts(now)...)
	out = append(out, a.witnessFacts(now)...)
	out = append(out, a.logstoreFacts(ctx)...)
	out = append(out, a.certFacts(now)...)
	out = append(out, a.listenFacts()...)
	out = append(out, a.snapshotFacts(ctx, now)...)
	a.pollLogEvents(ctx, now)
	return out
}

// meshFacts：到其他节点的路径为 none（面板只列出在线的节点；本节点也要连着面板，排除自己离线）。
// 丢包和延迟只用已经测到的。
func (a *agent) meshFacts(now time.Time) []alerts.Fact {
	n := a.mesh
	if n == nil || !a.panelConnected() {
		return nil
	}
	nodes := n.nodePeers()
	var out []alerts.Fact
	for _, p := range mesh.StatusProto(n.m, n.name).GetPeers() {
		var k mesh.Key
		copy(k[:], p.GetPublicKey())
		if !nodes[k] {
			continue // 面板，或者面板没再列出的成员（离线、已移除）
		}
		id := hex.EncodeToString(p.GetPublicKey()[:8])
		down := p.GetPath() == "none" || p.GetPath() == ""
		f := alerts.Fact{Key: "mesh_peer:" + id, Rule: alert.RuleMeshPeer, Subject: p.GetName()}
		if down {
			f.Bad = true
		} else {
			f.Good = true
		}
		// mesh_peer 的阈值是秒数：断开要持续这么久。状态机的 for_seconds 不用，这里按阈值换算。
		out = append(out, f)
		if !down {
			if p.GetTxLossKnown() {
				out = append(out, alerts.Fact{Key: "mesh_loss:" + id, Rule: alert.RuleMeshLoss, Subject: p.GetName(), Value: float64(p.GetTxLossPpm()) / 1e4, HasValue: true})
			}
			if p.GetRttMs() > 0 {
				out = append(out, alerts.Fact{Key: "mesh_rtt:" + id, Rule: alert.RuleMeshRTT, Subject: p.GetName(), Value: p.GetRttMs(), HasValue: true})
			}
		}
	}
	return out
}

// panelConnected 判断本节点现在连着至少一个面板。
func (a *agent) panelConnected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.panels) > 0
}

// witnessFacts：本节点是持有者或热备时，多久没收到见证人的回应。
func (a *agent) witnessFacts(now time.Time) []alerts.Fact {
	f := a.failovers
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alerts.Fact
	for _, st := range f.m {
		cfg := st.cfg
		if cfg == nil || len(cfg.GetWitness().GetKey()) == 0 {
			continue
		}
		if me, _ := a.selfParticipant(cfg); me == nil {
			continue // 本节点就是见证人
		}
		since := st.witnessSeen
		if since.IsZero() {
			if st.watchSince.IsZero() {
				st.watchSince = now
			}
			since = st.watchSince
		}
		silent := now.Sub(since)
		key := "witness:" + cfg.GetProjectId() + "/" + cfg.GetContainer()
		x := alerts.Fact{Key: key, Rule: alert.RuleWitness, Subject: describeContainer(cfg.GetContainer()), Project: cfg.GetProjectId(), Container: cfg.GetContainer()}
		// 节点规则：阈值是秒数（连续多久没有回应算失联）。
		limit := time.Duration(a.alerts.NodeRule(alert.RuleWitness).GetThreshold()) * time.Second
		x.Project, x.Container = "", "" // 见证人是节点规则，项目只用来显示
		if silent >= limit {
			x.Bad = true
		} else if !st.witnessSeen.IsZero() && now.Sub(st.witnessSeen) < time.Minute {
			x.Good = true
		}
		out = append(out, x)
	}
	return out
}

func describeContainer(name string) string { return "容器 " + name }

// witnessResponded 在收到见证人的投票结果时调用（同意或不同意都算见证人可达）。
func (a *agent) witnessResponded(cfg *nagiv1.FailoverConfig) {
	f := a.failovers
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.m[unitOf(cfg)]; st != nil {
		st.witnessSeen = time.Now()
	}
}

// logstoreFacts：日志库容量（disk/budget）；写入失败按增量计数（Count），进程重启后计数从 0 开始。
func (a *agent) logstoreFacts(ctx context.Context) []alerts.Fact {
	logs := a.cfg.Logs
	if logs == nil {
		return nil
	}
	st, err := logs.State(ctx)
	if err != nil {
		return nil
	}
	f := &a.facts
	f.mu.Lock()
	if f.logSeen && st.WriteErrors > f.logErrors {
		now := time.Now()
		for range min(st.WriteErrors-f.logErrors, 100) {
			a.alerts.Count("logstore_write", alert.RuleLogWrite, "", "", "", now)
		}
	}
	f.logErrors, f.logSeen = st.WriteErrors, true
	f.mu.Unlock()
	var out []alerts.Fact
	// 后台迁移（目前只有监控历史会写 failed，见 metrics/history.go）：失败告警，完成后恢复。
	for _, m := range logs.Migrations() {
		switch m.Phase {
		case "failed":
			out = append(out, alerts.Fact{Key: "migration:" + m.ID, Rule: alert.RuleMigration, Subject: migrationName(m.ID), Bad: true, Message: m.Error})
		case "complete":
			out = append(out, alerts.Fact{Key: "migration:" + m.ID, Rule: alert.RuleMigration, Subject: migrationName(m.ID), Good: true})
		}
	}
	if st.BudgetBytes > 0 {
		out = append(out, alerts.Fact{Key: "logstore_full", Rule: alert.RuleLogFull, Value: math.Min(100, 100*float64(st.DiskBytes)/float64(st.BudgetBytes)), HasValue: true})
	}
	return out
}

// certFacts：节点证书续期失败，且距到期不到阈值天数，或连续失败 3 次；续期成功后恢复。单纯快到期（还没失败）不算。
func (a *agent) certFacts(now time.Time) []alerts.Fact {
	f := &a.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.certExpiry.IsZero() {
		return nil
	}
	days := math.Floor(f.certExpiry.Sub(now).Hours() / 24)
	limit := a.alerts.NodeRule(alert.RuleNodeCert).GetThreshold()
	x := alerts.Fact{Key: "node_cert", Rule: alert.RuleNodeCert, Value: days, HasValue: true, Judged: true, Message: f.certErr,
		Bad: f.certFails > 0 && (days < limit || f.certFails >= 3), Good: f.certFails == 0}
	return []alerts.Fact{x}
}

// certRenewed 记下一次续期的结果。
func (a *agent) certRenewed(expiry time.Time, err error) {
	f := &a.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	f.certExpiry = expiry
	if err != nil {
		f.certFails++
		f.certErr = err.Error()
	} else {
		f.certFails, f.certErr = 0, ""
	}
}

// bindError 从 Caddy 的加载错误里找出监听失败的地址，例如 "listen tcp :27015: bind: address already in use"。
var bindError = regexp.MustCompile(`listen (tcp|udp)[46]? [^ ]*:(\d+): bind: ([^"]+)`)

// proxyApplied 记下一次加载 Caddy 配置的结果：监听失败时按端口找到映射；成功时清空。
func (a *agent) proxyApplied(routes []routeLike, err error) {
	f := &a.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		f.listen, f.listenOK = nil, time.Now()
		return
	}
	m := bindError.FindStringSubmatch(err.Error())
	if m == nil {
		return // 不是端口冲突（配置错误等），不归到某个映射
	}
	port, _ := strconv.Atoi(m[2])
	for _, r := range routes {
		if r.protocol == m[1] && int(r.published) == port {
			if f.listen == nil {
				f.listen = map[string]string{}
			}
			f.listen[r.project+"/"+r.name] = m[1] + "/" + m[2] + ": " + strings.TrimSpace(m[3])
		}
	}
}

// routeLike 是判断端口归属用到的映射字段。
type routeLike struct {
	project, name, protocol string
	published               uint32
}

// listenFacts：端口监听失败的映射是 bad；加载成功后，之前失败过的映射是 good（其余没有证据）。
func (a *agent) listenFacts() []alerts.Fact {
	f := &a.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alerts.Fact
	for route, msg := range f.listen {
		project, name, _ := strings.Cut(route, "/")
		out = append(out, alerts.Fact{Key: alert.RuleL4Listen + ":" + route, Rule: alert.RuleL4Listen, Subject: name, Project: project, Bad: true, Message: msg})
	}
	if len(f.listen) == 0 && !f.listenOK.IsZero() {
		for _, key := range a.alerts.FiringKeys(alert.RuleL4Listen + ":") {
			route := strings.TrimPrefix(key, alert.RuleL4Listen+":")
			project, name, _ := strings.Cut(route, "/")
			out = append(out, alerts.Fact{Key: key, Rule: alert.RuleL4Listen, Subject: name, Project: project, Good: true})
		}
	}
	return out
}

// snapshotFacts：设置了定时快照的容器，距最近一次成功快照超过 threshold 个间隔；有备份节点或热备的容器，
// 距最近一次成功同步超过 threshold 个快照间隔（热备不超过 10 分钟一个间隔）。要先满 threshold 个间隔才判断。
func (a *agent) snapshotFacts(ctx context.Context, now time.Time) []alerts.Fact {
	var out []alerts.Fact
	for _, c := range a.projects.List(ctx) {
		s := c.Spec
		sch := spec.SnapshotScheduleOf(s)
		if sch.GetDisabled() {
			continue
		}
		gap := spec.SnapshotGap(sch, 0)
		pid, name := s.GetProjectId(), s.GetName()
		subject := describeContainer(name)
		n := a.alerts.ContainerRule(pid, name, alert.RuleSnapshotStale).GetThreshold()
		limit := time.Duration(n * float64(gap))
		if last := c.LastSnapshot; last.Unix() > 0 {
			age := now.Sub(last)
			out = append(out, alerts.Fact{Key: alert.RuleSnapshotStale + ":" + pid + "/" + name, Rule: alert.RuleSnapshotStale, Subject: subject,
				Project: pid, Container: name, Bad: age > limit, Good: age <= gap})
		}
		if targets := backupTargets(s); len(targets) > 0 {
			m := a.alerts.ContainerRule(pid, name, alert.RuleBackupLag).GetThreshold()
			lagLimit := time.Duration(m * float64(gap))
			for _, st := range a.backups.statuses(s) {
				if st.GetLastSync() == nil || st.GetSyncing() || st.GetStopped() {
					continue
				}
				age := now.Sub(st.GetLastSync().AsTime())
				out = append(out, alerts.Fact{Key: alert.RuleBackupLag + ":" + pid + "/" + name + "/" + st.GetTargetNodeId(), Rule: alert.RuleBackupLag,
					Subject: subject + " → " + st.GetTargetName(), Project: pid, Container: name, Bad: age > lagLimit, Good: age <= gap})
			}
		}
	}
	// 热备：持有者同步给热备的时间。
	if f := a.failovers; f != nil {
		f.mu.Lock()
		for _, st := range f.m {
			cfg := st.cfg
			if cfg == nil || st.lastSync.IsZero() {
				continue
			}
			if me, _ := a.selfParticipant(cfg); me == nil || string(me.GetKey()) != string(st.holder) {
				continue
			}
			pid, name := cfg.GetProjectId(), cfg.GetContainer()
			gap := 10 * time.Minute
			limit := time.Duration(a.alerts.ContainerRule(pid, name, alert.RuleBackupLag).GetThreshold() * float64(gap))
			age := now.Sub(st.lastSync)
			out = append(out, alerts.Fact{Key: alert.RuleBackupLag + ":" + pid + "/" + name + "/standby", Rule: alert.RuleBackupLag,
				Subject: describeContainer(name) + "（热备）", Project: pid, Container: name, Bad: age > limit, Good: age <= gap})
		}
		f.mu.Unlock()
	}
	return out
}

// ---- 次数类：日志里的事件 ----

// pollLogEvents 读本机日志库里新到的内核磁盘错误、内核 OOM 和容器 OOM 事件，各记一次（见 Engine.Count）。
// 从节点启动的时间开始读，不把以前的事件当成新的。
func (a *agent) pollLogEvents(ctx context.Context, now time.Time) {
	logs := a.cfg.Logs
	if logs == nil {
		return
	}
	f := &a.facts
	// Each read is limited to its category's index and moves the cursor to the
	// page watermark even when nothing matched. An unfiltered prefix scan from
	// the start time held the shared log gate for seconds every round and made
	// the alert and audit writes queued behind it time out.
	f.mu.Lock()
	if f.logAfter == nil {
		st, err := logs.State(ctx)
		if err != nil {
			f.mu.Unlock()
			return
		}
		f.logAfter = map[string]int64{logstore.Runtime: st.Last, logstore.Task: st.Last}
	}
	after := maps.Clone(f.logAfter)
	f.mu.Unlock()
	// Host journal lines are Runtime records; Docker engine events are Task records.
	for _, src := range []struct{ category, prefix string }{{logstore.Runtime, "host.kernel."}, {logstore.Task, "docker.container.oom"}} {
		page, err := logs.ReadPage(ctx, logstore.Filter{Category: src.category, After: after[src.category], KindPrefix: src.prefix, Limit: 1000})
		if err != nil {
			continue
		}
		for _, e := range page.Events {
			a.countLogEvent(e)
		}
		after[src.category] = page.Next
	}
	f.mu.Lock()
	for c, seq := range after {
		f.logAfter[c] = max(f.logAfter[c], seq)
	}
	f.mu.Unlock()
}

// countLogEvent 按事件记一次：内核磁盘错误、内核 OOM（只算杀进程的，不含 lockup、hung task）、容器 OOM。
func (a *agent) countLogEvent(e logstore.Event) {
	count := a.alerts.Count
	if a.countAlert != nil {
		count = a.countAlert
	}
	at := e.Time
	if at.IsZero() {
		at = time.Now()
	}
	var data struct {
		Message    string            `json:"message"`
		Attributes map[string]string `json:"attributes"`
	}
	if raw, ok := e.Data.(json.RawMessage); ok {
		_ = json.Unmarshal(raw, &data)
	}
	switch e.Kind {
	case "host.kernel.storage":
		count("kernel_storage", alert.RuleKernelStorage, "", "", "", at)
	case "host.kernel.memory":
		if containsAny(strings.ToLower(data.Message), "oom-kill", "out of memory", "killed process") {
			count("kernel_oom", alert.RuleKernelOOM, "", "", "", at)
		}
	case "docker.container.oom":
		container := data.Attributes["nagi.container"]
		if e.Project == "" || container == "" {
			return
		}
		count(alert.RuleContainerOOM+":"+e.Project+"/"+container, alert.RuleContainerOOM, describeContainer(container), e.Project, container, at)
	}
}

func containsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// migrationName 是迁移的显示名。
func migrationName(id string) string {
	if id == "metrics" {
		return "监控历史"
	}
	return id
}
