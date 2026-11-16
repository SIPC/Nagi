package agent

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
)

// egress 维护出站限制的规则，读内核日志发告警（见 egress.go）。
type egress struct {
	a *agent

	mu      sync.Mutex
	err     string // 不能限制时的原因，随节点信息上报
	applied string
	// byIP 是最近一次生成规则时容器的地址 → 项目、容器名，读日志时找是哪个容器。
	byIP   map[string]egressContainer
	modes  map[string]string
	alerts egressAlerts
	kick   chan struct{}
}

func newEgress(a *agent) *egress { return &egress{a: a, kick: make(chan struct{}, 1)} }

// Error 返回不能限制容器出站的原因，为空表示可以。
func (e *egress) Error() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func (e *egress) trigger() {
	if e == nil {
		return
	}
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *egress) setErr(s string) {
	e.mu.Lock()
	changed := e.err != s
	e.err = s
	e.mu.Unlock()
	if changed {
		e.a.changed()
	}
}

func (e *egress) run(ctx context.Context) {
	if _, err := exec.LookPath("nft"); err != nil {
		e.setErr(i18n.New("这台机器没有 nftables（nft 命令），无法限制容器出站").Error())
		e.a.log.Warn("没有 nftables，无法限制容器出站")
		return
	}
	go e.watchHits(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		e.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		case <-e.a.intranet.planChanged():
		}
	}
}

// sync 按现在的容器和代理计划生成规则，有变化时应用。
func (e *egress) sync(ctx context.Context) {
	st, err := egressStateOf(ctx, e.a.docker, e.a.intranet.current())
	if err != nil {
		return
	}
	rules := buildRuleset(st)
	byIP := map[string]egressContainer{}
	for _, c := range st.containers {
		if spec.EgressMode(st.projects[c.project]) == spec.EgressOff {
			continue
		}
		for _, a := range c.addrs {
			byIP[a.String()] = c
		}
	}
	modes := map[string]string{}
	for id, p := range st.projects {
		modes[id] = spec.EgressMode(p)
	}
	e.mu.Lock()
	e.byIP, e.modes = byIP, modes
	same := rules == e.applied
	e.mu.Unlock()
	if same {
		return
	}
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.a.log.Warn("应用出站限制失败", "err", err, "output", strings.TrimSpace(string(out)))
		e.setErr(i18n.Errorf("无法应用出站限制的规则：%s", strings.TrimSpace(string(out))).Error())
		return
	}
	e.mu.Lock()
	e.applied = rules
	e.mu.Unlock()
	e.setErr("")
	e.a.log.Info("已更新容器出站限制", "containers", len(st.containers))
}

// watchHits 每 5 秒读一次命中集合，新出现的连接汇总成告警（同一个容器、同一个目标 10 分钟内一次）。
func (e *egress) watchHits(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, set := range []string{"hits4", "hits6"} {
			out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", "nagi", set).Output()
			if err != nil {
				continue
			}
			for _, h := range parseHits(string(out)) {
				e.hit(ctx, h)
			}
		}
	}
}

func (e *egress) hit(ctx context.Context, h egressHit) {
	e.mu.Lock()
	c, ok := e.byIP[h.src.String()]
	mode := e.modes[c.project]
	e.mu.Unlock()
	if !ok {
		return
	}
	target := net.JoinHostPort(h.dst.String(), strconv.Itoa(int(h.port)))
	key := c.name + ">" + target
	if !e.alerts.first(key, time.Now()) {
		return
	}
	name := c.nagi
	if name == "" {
		name = c.name
	}
	title := i18n.Errorf("容器 %s 尝试连接 %s，已拦截", name, target).Error()
	if mode != spec.EgressBlock {
		title = i18n.Errorf("容器 %s 尝试连接 %s，未拦截", name, target).Error()
	}
	e.a.log.Warn("容器出站", "container", c.name, "target", target, "mode", mode)
	// 键是 egress:<项目>/<容器>/<目标>：面板按它把告警记到项目和容器上（见 nodes.alertContainer）。
	e.a.alerts.NotifyKey(ctx, "egress:"+c.project+"/"+name+"/"+target, title, "", c.project)
}
