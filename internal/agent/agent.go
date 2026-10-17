// Package agent 是节点：保存自己项目的数据，主动连接信任的面板，接受面板的反向调用。
package agent

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
)

// Config 是节点的配置。
type Config struct {
	// DataDir 是节点目录，默认 /var/lib/nagi。
	DataDir string
	// Panel、Fingerprint、Token 用于首次加入面板；已经信任该面板时 Token 可以为空。
	Panel       string
	Fingerprint string
	Token       string
	// SubnetPool 是节点本地的容器网段池。
	SubnetPool string
	// DisableProxy 不启动内嵌的 Caddy，只用于开发（例如在 Windows 上调试）。
	DisableProxy bool
	// MeshPort 是 mesh 的 UDP 端口，0 表示用记住的端口（第一次随机选）。
	MeshPort uint16
	// Embedded 表示这是面板自带的节点：随面板一起升级，不能单独升级。
	Embedded bool
	Version  string
	Logger   *slog.Logger
}

// Enrolled 判断节点是否已经信任某个面板，用于决定是否需要生成加入令牌。
func Enrolled(dataDir, fingerprint string) bool {
	id, err := loadIdentity(filepath.Join(dataDir, "identity"))
	return err == nil && id.trusts(fingerprint)
}

// Enroll 只加入面板（把面板加入信任列表）然后返回，不运行节点。安装脚本用它，
// 这样一次性令牌不会写进服务配置。
func Enroll(ctx context.Context, cfg Config) error {
	if cfg.Panel == "" || cfg.Fingerprint == "" || cfg.Token == "" {
		return errors.New("需要 --server、--fingerprint 和 --token")
	}
	id, err := loadIdentity(filepath.Join(cfg.DataDir, "identity"))
	if err != nil {
		return fmt.Errorf("读取节点身份: %w", err)
	}
	if id.trusts(cfg.Fingerprint) {
		fmt.Println("节点已经信任这个面板，无需重复加入")
		return nil
	}
	hostname, _ := os.Hostname()
	if err := id.enroll(ctx, cfg.Panel, cfg.Fingerprint, cfg.Token, hostname); err != nil {
		return err
	}
	fmt.Println("已加入面板", cfg.Panel)
	return nil
}

// Run 运行节点，直到 ctx 取消。
func Run(ctx context.Context, cfg Config) error {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.SubnetPool == "" {
		cfg.SubnetPool = project.DefaultSubnetPool
	}

	id, err := loadIdentity(filepath.Join(cfg.DataDir, "identity"))
	if err != nil {
		return fmt.Errorf("读取节点身份: %w", err)
	}
	hostname, _ := os.Hostname()
	if cfg.Panel != "" && cfg.Fingerprint != "" && !id.trusts(cfg.Fingerprint) {
		if cfg.Token == "" {
			return errors.New("节点尚未加入这个面板，需要提供加入令牌")
		}
		if err := id.enroll(ctx, cfg.Panel, cfg.Fingerprint, cfg.Token, hostname); err != nil {
			return err
		}
		log.Info("已加入面板", "panel", cfg.Panel)
	}
	if len(id.Panels()) == 0 {
		// 项目照常运行；用本地命令 nagi panel add 加入面板后会自动连接。
		log.Warn("节点没有信任任何面板，等待用 nagi panel add 加入")
	}

	nodeDB, err := db.OpenMigrated(ctx, filepath.Join(cfg.DataDir, "node.db"), nodestore.Migrations)
	if err != nil {
		return fmt.Errorf("打开节点数据库: %w", err)
	}
	defer nodeDB.Close()

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("连接 Docker: %w", err)
	}
	defer dc.Close()

	projects, err := project.NewManager(nodeDB, dc, cfg.SubnetPool, filepath.Join(cfg.DataDir, "snapshots"))
	if err != nil {
		return err
	}

	a := &agent{
		cfg:       cfg,
		log:       log,
		id:        id,
		docker:    dc,
		projects:  projects,
		hostname:  hostname,
		reload:    make(chan struct{}, 1),
		replicas:  &replicas{dir: filepath.Join(cfg.DataDir, "replicas")},
		failovers: newFailovers(cfg.DataDir),
		ctx:       ctx,
	}
	a.failovers.loadAll()
	projects.FastSnapshot = func(id string) bool { return a.failovers.get(id) != nil }
	projects.ManagedRestart = func(id string) bool { return a.failovers.get(id) != nil }
	projects.ReservedDomain = func(d string) bool { return d != "" && d == a.panelDomain() }
	a.startedAt = time.Now()
	a.info = a.nodeInfo(ctx)

	// 容器有变化时上报状态；容器重建后要重新接入项目内网和共享网络（见 project/network.go）。
	netSync := make(chan struct{}, 1)
	go dc.WatchProjects(ctx, project.LabelProject, func() {
		a.changed()
		select {
		case netSync <- struct{}{}:
		default:
		}
	})
	go func() {
		projects.SyncAllNetworks(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-netSync:
				time.Sleep(time.Second) // 一次部署会连续产生很多事件，攒一下再处理
				projects.SyncAllNetworks(ctx)
			}
		}
	}()
	a.metrics = metrics.New(nodeDB, dc, cfg.DataDir, log.With("component", "metrics"))
	a.metrics.ExtraRestarts = projects.ManagedRestartCount
	if !cfg.DisableProxy {
		a.metrics.AddSource(routeMetrics())
	}
	go a.metrics.Run(ctx)
	a.alerts = alerts.New(log.With("component", "alerts"), hostname, filepath.Join(cfg.DataDir, "identity"),
		filepath.Join(cfg.DataDir, "caddy"), a.metrics, dc,
		func(id string) string { return projects.Name(context.Background(), id) }, a.relayAlert)
	go a.alerts.Run(ctx)
	if err := a.startMesh(ctx, cfg.MeshPort); err != nil {
		log.Warn("mesh 启动失败，依赖 mesh 的功能不可用", "err", err)
	} else {
		for id := range a.failovers.m {
			a.startFailover(ctx, id)
		}
	}
	proxy.OnCertEvent(func(domain string, err error) {
		// Caddy 在申请证书的过程中同步调用，发送告警放到后台。
		if err == nil {
			go a.alerts.Resolve(ctx, "cert_failed:"+domain)
			return
		}
		go a.alerts.Fire(ctx, "cert_failed:"+domain, fmt.Sprintf("域名 %s 申请证书失败", domain), err.Error(),
			a.domainProject(ctx, domain))
	})
	go a.snapshotLoop(ctx)
	go a.imageCheckLoop(ctx)

	if !cfg.DisableProxy {
		a.proxy = proxy.New(filepath.Join(cfg.DataDir, "caddy"), log.With("component", "proxy"))
		a.proxyKick = make(chan struct{}, 1)
		defer a.proxy.Stop()
		go a.proxyLoop(ctx)
	}

	a.superviseLoop(ctx)
	return nil
}

type agent struct {
	cfg      Config
	log      *slog.Logger
	id       *identity
	docker   *docker.Client
	projects *project.Manager
	hostname string
	info     *nagiv1.NodeInfo

	proxy     *proxy.Proxy
	proxyKick chan struct{}
	metrics   *metrics.Collector
	alerts    *alerts.Engine
	mesh      *meshNode
	replicas  *replicas
	failovers *failovers
	ctx       context.Context
	startedAt time.Time

	mu       sync.Mutex
	subs     []chan struct{}
	panels   map[string]nagiv1.PanelServiceClient // 面板指纹 → 当前连接，用于请面板代发告警
	sessions map[string]context.CancelFunc        // 面板指纹 → 停止连接
	syncing  map[string]bool                      // 正在同步到备份节点的项目
	reload   chan struct{}
}

// superviseLoop 按信任列表维持到每个面板的连接：列表变了（面板发指令、或者本地命令改了文件）就增减连接。
func (a *agent) superviseLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		if err := a.id.reload(); err != nil {
			a.log.Warn("读取信任列表失败", "err", err)
		}
		a.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.reload:
		}
	}
}

func (a *agent) reconcile(ctx context.Context) {
	want := map[string]trustedPanel{}
	for _, p := range a.id.Panels() {
		want[p.Fingerprint] = p
	}
	a.mu.Lock()
	if a.sessions == nil {
		a.sessions = map[string]context.CancelFunc{}
	}
	for fp, p := range want {
		if a.sessions[fp] == nil {
			sctx, cancel := context.WithCancel(ctx)
			a.sessions[fp] = cancel
			go a.connectLoop(sctx, fp)
			a.log.Info("开始连接面板", "panel", p.Address)
		}
	}
	for fp, cancel := range a.sessions {
		if _, ok := want[fp]; !ok {
			cancel()
			delete(a.sessions, fp)
			a.log.Info("已移除面板，断开连接", "fingerprint", fp)
		}
	}
	a.mu.Unlock()
	keep := make([]string, 0, len(want))
	for fp := range want {
		keep = append(keep, fp)
	}
	a.alerts.Retain(keep)
	a.mesh.retain(keep)
}

// reloadPanels 让 superviseLoop 立即按信任列表调整连接。
func (a *agent) reloadPanels() {
	select {
	case a.reload <- struct{}{}:
	default:
	}
}

// relayAlert 请指定的面板代发告警。
func (a *agent) relayAlert(ctx context.Context, panel string, al *nagiv1.Alert, channelIDs []string) error {
	a.mu.Lock()
	c := a.panels[panel]
	a.mu.Unlock()
	if c == nil {
		return errors.New("面板不可达")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := c.RelayAlert(ctx, &nagiv1.RelayAlertRequest{Alert: al, ChannelIds: channelIDs})
	return err
}

// proxyLoop 在项目或容器变化时重新生成 Caddy 配置；另外每 15 秒检查一次，及时发现新拿到的证书。
func (a *agent) proxyLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		routes, err := a.projects.Routes(ctx)
		routes = append(routes, a.panelRoutes()...)
		if err == nil {
			err = a.proxy.Apply(routes)
		}
		if err != nil && ctx.Err() == nil {
			a.log.Warn("更新对外访问规则失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.proxyKick:
			// 合并短时间内连续的变化。
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// changed 通知所有面板连接：项目状态有变化，需要重新上报。
func (a *agent) changed() {
	if a.proxyKick != nil {
		select {
		case a.proxyKick <- struct{}{}:
		default:
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *agent) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subs = append(a.subs, ch)
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, c := range a.subs {
			if c == ch {
				a.subs = append(a.subs[:i], a.subs[i+1:]...)
				return
			}
		}
	}
}

func (a *agent) nodeInfo(ctx context.Context) *nagiv1.NodeInfo {
	info := &nagiv1.NodeInfo{
		Hostname: a.hostname,
		Os:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Version:  a.cfg.Version,
		Embedded: a.cfg.Embedded,
	}
	if v, err := a.docker.Version(ctx); err == nil {
		info.DockerVersion = v
	} else {
		a.log.Warn("无法连接 Docker", "err", err)
	}
	if v, err := compose.Version(ctx); err == nil {
		info.ComposeVersion = v
	} else {
		a.log.Warn("无法执行 docker compose", "err", err)
	}
	return info
}

// connectLoop 保持与一个面板的连接，断开后按指数退避重连。每次连接都读取信任列表里最新的条目
// （地址或证书可能已经更新），面板被移出列表后退出。
func (a *agent) connectLoop(ctx context.Context, fingerprint string) {
	backoff := time.Second
	for ctx.Err() == nil {
		p, ok := a.id.panel(fingerprint)
		if !ok {
			return
		}
		start := time.Now()
		err := a.session(ctx, p)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("与面板的连接已断开，稍后重连", "panel", p.Address, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session 建立一次控制连接：TLS + yamux，在上面同时运行 AgentService 服务端和 PanelService 客户端。
func (a *agent) session(ctx context.Context, p trustedPanel) error {
	tlsCfg, err := a.id.tlsConfig(p, tunnel.ALPN)
	if err != nil {
		return err
	}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, Config: tlsCfg}
	conn, err := dialer.DialContext(ctx, "tcp", p.Address)
	if err != nil {
		return err
	}
	sess, err := tunnel.Client(conn)
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()

	srv := grpc.NewServer()
	nagiv1.RegisterAgentServiceServer(srv, &agentService{
		node: a, projects: a.projects, metrics: a.metrics, alerts: a.alerts, panel: p.Fingerprint, changed: a.changed,
	})
	go tunnel.Serve(sess, srv)
	defer srv.Stop()

	cc, err := tunnel.Dial(sess)
	if err != nil {
		return err
	}
	defer cc.Close()
	panel := nagiv1.NewPanelServiceClient(cc)

	a.log.Info("已连接面板", "panel", p.Address)
	a.mu.Lock()
	if a.panels == nil {
		a.panels = map[string]nagiv1.PanelServiceClient{}
	}
	a.panels[p.Fingerprint] = panel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.panels[p.Fingerprint] == panel {
			delete(a.panels, p.Fingerprint)
		}
		a.mu.Unlock()
	}()
	a.renewIfNeeded(ctx, panel, p)
	notify, unsubscribe := a.subscribe()
	defer unsubscribe()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	relaying := false
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		resp, err := a.report(ctx, panel)
		if err != nil {
			return err
		}
		if key, ok := a.mesh.update(p.Fingerprint, p.Address, resp.GetMesh()); ok && !relaying {
			relaying = true
			go a.mesh.relayLoop(sctx, panel, key)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-sess.CloseChan():
			return errors.New("连接已关闭")
		case <-ticker.C:
		case <-notify:
			// 短暂合并连续的变化，避免频繁上报。
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (a *agent) report(ctx context.Context, panel nagiv1.PanelServiceClient) (*nagiv1.ReportResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	projects, err := a.projects.List(ctx)
	if err != nil {
		a.log.Warn("读取项目列表失败", "err", err)
	}
	return panel.Report(ctx, &nagiv1.ReportRequest{Node: a.info, Projects: projects, Mesh: a.mesh.info(),
		BackupKeys: a.projects.BackupKeys(ctx)})
}

// renewIfNeeded：证书 30 天内到期时请面板续期。新证书在下次连接时生效。
func (a *agent) renewIfNeeded(ctx context.Context, panel nagiv1.PanelServiceClient, p trustedPanel) {
	cert, err := p.certificate()
	if err != nil || time.Until(cert.NotAfter) > 30*24*time.Hour {
		return
	}
	csr, err := tlsutil.CreateCSR(a.id.key, a.hostname)
	if err != nil {
		return
	}
	resp, err := panel.RenewCertificate(ctx, &nagiv1.RenewCertificateRequest{Csr: csr})
	if err != nil {
		a.log.Warn("证书续期失败", "panel", p.Address, "err", err)
		return
	}
	p.CertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.GetCertificate()}))
	if err := a.id.addPanel(p); err != nil {
		a.log.Warn("保存续期后的证书失败", "err", err)
		return
	}
	a.log.Info("证书已续期", "panel", p.Address)
}

// domainProject 返回绑定了这个域名的项目 ID，找不到时返回空。
func (a *agent) domainProject(ctx context.Context, domain string) string {
	routes, _ := a.projects.Routes(ctx)
	for _, r := range routes {
		if slices.Contains(r.Domains, domain) {
			return r.Project
		}
	}
	return ""
}

// routeMetrics 把 Caddy 统计的请求转成监控序列。有过请求的规则，之后没有请求时记 0，
// 超过一天没有请求就不再记录（规则可能已经删除）。
func routeMetrics() metrics.Source {
	lastActive := map[string]time.Time{}
	return func(elapsed float64) map[string]float64 {
		now := time.Now()
		stats := proxy.TakeStats()
		for k, s := range stats {
			if s.Requests > 0 {
				lastActive[k] = now
			}
		}
		out := map[string]float64{}
		for k, t := range lastActive {
			if now.Sub(t) > 24*time.Hour {
				delete(lastActive, k)
				continue
			}
			s := stats[k]
			prefix := "r:" + k + ":"
			out[prefix+"req"] = float64(s.Requests) / elapsed
			out[prefix+"5xx"] = float64(s.Errors) / elapsed
			if s.Requests > 0 {
				out[prefix+"lat"] = 1000 * s.Seconds / float64(s.Requests)
			}
		}
		return out
	}
}

// snapshotLoop 每分钟检查一次，给到时间的项目做定时快照。
func (a *agent) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.projects.SnapshotDue(ctx, func(id string, err error) {
			if err == nil {
				a.alerts.Resolve(ctx, "snapshot:"+id)
				// 定时快照成功后同步到备份节点和热备（没设置时什么都不做）。
				go a.runBackupSync(ctx, id)
				if st := a.failovers.get(id); st != nil {
					go a.syncStandby(ctx, id)
				}
				return
			}
			a.log.Warn("定时快照失败", "project", id, "err", err)
			a.alerts.Fire(ctx, "snapshot:"+id, "定时快照失败", err.Error(), id)
		})
	}
}
