package agent

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
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

// egressCacheFile 是上次应用的规则（见 restoreEgress）。
func egressCacheFile(dataDir string) string { return filepath.Join(dataDir, "egress.nft") }

// restoreEgress 在节点启动时最先加载上次应用的规则（见 docs/agent.md#出站限制）：节点重启后 dockerd 先拉起容器，
// nagi 起来、列完容器之前，容器的出站不受限制，恶意镜像在入口程序里立刻访问元数据就能成功。上次的规则里的地址
// 可能已经变了，等第一次同步按现在的容器重新生成；在这之前宁可多拦。没有 nft 或没有缓存时什么也不做。
func restoreEgress(dataDir string, log *slog.Logger) {
	b, err := os.ReadFile(egressCacheFile(dataDir))
	if err != nil || len(b) == 0 {
		return
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(string(b))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Warn("加载上次的出站限制规则失败", "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	log.Info("已加载上次的出站限制规则，等列完容器后更新")
}

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
	if e.a.proxy != nil {
		st.public = e.a.proxy.PublicPorts()
		sort.Slice(st.public, func(i, j int) bool {
			return st.public[i].Proto < st.public[j].Proto || st.public[i].Proto == st.public[j].Proto && st.public[i].Port < st.public[j].Port
		})
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
	// 存下来，下次启动时最先加载（见 restoreEgress）。
	if err := writeFileAtomic(egressCacheFile(e.a.cfg.DataDir), []byte(rules), 0o600); err != nil {
		e.a.log.Warn("无法保存出站限制规则", "err", err)
	}
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
	// 告警可以在项目或容器的告警规则里关掉（规则 egress），关掉后照常拦截，只记日志。
	if !e.a.alerts.ContainerEnabled(c.project, name, alert.RuleEgress) {
		return
	}
	// 键是 egress:<项目>/<容器>/<目标>：面板按它把告警记到项目和容器上（见 nodes.alertContainer）。
	e.a.alerts.NotifyKey(ctx, "egress:"+c.project+"/"+name+"/"+target, title, "", c.project)
}

// writeFileAtomic 先写临时文件再改名，写到一半断电也不会留下半份规则。
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
