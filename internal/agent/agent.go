// Package agent 是节点：按面板下发的配置运行容器并缓存配置，主动连接信任的面板，接受面板的反向调用。
package agent

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/alerts"
	"github.com/SIPC/nagi/internal/agent/auditlog"
	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/logging"
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/recording"
	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
	// PanelRelay 只给面板自带的节点：面板替节点中转的累计字节数（收、发），记成 m:relay:rx、:tx。
	// 这些流量经过本机网卡，但不是本机和谁之间的，不记就会算成对外流量。
	PanelRelay func() (rx, tx uint64)
	Version    string
	Logger     *slog.Logger
	// Logs 只给内嵌节点复用面板本机库（和已经接入该库的 Logger），避免同一台机器分配两份容量。
	// 独立节点从 DataDir/logs/config.json 读取配置并自行打开。
	Logs      *logstore.Store
	LogConfig logstore.Config
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
func Run(ctx context.Context, cfg Config) (runErr error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.SubnetPool == "" {
		cfg.SubnetPool = project.DefaultSubnetPool
	}

	// 先加载上次的出站限制规则：dockerd 可能已经拉起了容器，规则不能等到列完容器才生效（见 egress_linux.go）。
	restoreEgress(cfg.DataDir, log)

	// 刚升级时最先开始自检：后面任何一步出错、卡住，都能换回旧版本（见 upgrade_guard.go）。
	confirmUpgrade := func() {}
	if !cfg.Embedded {
		confirmUpgrade = guardUpgrade(filepath.Join(cfg.DataDir, "identity"), cfg.Version, log)
	}
	if cfg.Logs == nil && !cfg.Embedded {
		var err error
		cfg.Logs, cfg.LogConfig, err = logstore.OpenLocal(ctx, filepath.Join(cfg.DataDir, "logs"))
		if err != nil {
			log.Error("无法打开本机日志，继续运行；新日志未入库，请检查磁盘和日志配置", "err", err)
		} else {
			defer cfg.Logs.Close()
			log = cfg.Logs.Logger(log, cfg.LogConfig)
		}
	}

	started := time.Now()
	cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "node.starting", Data: map[string]any{"version": cfg.Version, "embedded": cfg.Embedded}})
	defer func() {
		data := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "result": "stopped", "context_canceled": ctx.Err() != nil}
		if runErr != nil {
			data["result"], data["error"] = "failed", runErr.Error()
		}
		cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "node.stopped", Data: data})
	}()
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

	var hadLegacyMetrics bool
	if err := nodeDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metrics_raw UNION ALL SELECT 1 FROM metrics_m5)`).Scan(&hadLegacyMetrics); err != nil {
		log.Warn("无法检查旧监控历史", "err", err)
	}
	metricDB := nodeDB
	if history, err := metrics.OpenHistory(ctx, filepath.Join(cfg.DataDir, "logs", "metrics.db"), nodeDB, cfg.Logs); err != nil {
		log.Error("无法分离监控历史，旧库仅供查询；暂停图表历史写入，重启后重试迁移", "err", err)
	} else {
		metricDB = history
		defer history.Close()
		if hadLegacyMetrics {
			st, statErr := os.Stat(filepath.Join(cfg.DataDir, "node.db"))
			if statErr != nil {
				log.Warn("监控历史已迁移，无法检查旧配置库大小", "err", statErr)
			} else if err := metrics.CheckDisk(cfg.DataDir, st.Size()); err != nil {
				log.Warn("监控历史已迁移，空间不足，暂不整理旧配置库", "err", err)
			} else if _, err := nodeDB.ExecContext(ctx, `VACUUM`); err != nil {
				log.Warn("监控历史已迁移，旧配置库空闲空间暂未回收", "err", err)
			}
		}
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("连接 Docker: %w", err)
	}
	defer dc.Close()

	projects, err := project.NewManager(nodeDB, dc, cfg.SubnetPool, filepath.Join(cfg.DataDir, "snapshots"))
	if err != nil {
		return err
	}

	projects.Journal = cfg.Logs
	a := &agent{
		cfg:      cfg,
		log:      log,
		id:       id,
		docker:   dc,
		projects: projects,
		hostname: hostname,
		reload:   make(chan struct{}, 1),
		ctx:      ctx,

		confirmUpgrade: confirmUpgrade,
	}
	a.startedAt = time.Now()
	a.files = a.newNodeFiles()
	a.info = a.nodeInfo(ctx)
	a.intranet = newIntranet(a)
	a.direct = newDirectDL(a)
	a.egress = newEgress(a)
	a.violations = newViolations(a)
	closeAudit := a.openAudit(ctx)
	defer closeAudit()
	closeRecordings := a.openRecordings(ctx)
	defer closeRecordings()
	if cfg.Logs != nil {
		logCtx, stopLogs := context.WithCancel(ctx)
		logsDone := make(chan struct{})
		hostDone := make(chan struct{})
		go func() {
			defer close(hostDone)
			logging.RunHost(logCtx, cfg.Logs, cfg.LogConfig)
		}()
		go func() {
			defer close(logsDone)
			logging.RunContainers(logCtx, dc, cfg.Logs, cfg.LogConfig)
		}()
		defer func() { stopLogs(); <-logsDone; <-hostDone }()
	}
	projects.EntryPorts = a.intranet.entryPorts
	projects.ProxyPorts = func() []spec.PortKey {
		if a.proxy == nil {
			return nil
		}
		return a.proxy.DomainPorts()
	}
	projects.ReservedDirs = []string{cfg.DataDir}
	if cfg.Embedded {
		projects.ReservedDirs = append(projects.ReservedDirs, filepath.Dir(cfg.DataDir))
	}
	projects.ProjectsRoot = a.projectsRoot
	a.replicas = newReplicas(cfg.DataDir)
	a.replicas.reserved = projects.ReservedDirs
	a.replicas.managed = func() []string { return a.projects.ManagedDirs(context.Background()) }
	a.replicas.projectsRoot = a.projectsRoot
	a.replicas.denied = a.files.DeniedBesidesFunc
	a.replicas.stateDir = a.replicaStateDir
	projects.ReplicaRoots = a.replicas.roots
	projects.ReplicaBases = a.replicas.bases
	projects.ReplicaIndexBroken = func() bool { return a.replicas.usable() != nil }
	a.backups = newBackupStates(filepath.Join(cfg.DataDir, "backup-status.json"))
	projects.BackupStatus = a.backups.statuses
	a.failovers = newFailovers(cfg.DataDir)
	a.failovers.history = logging.NewDecisions(cfg.Logs, cfg.LogConfig.DecisionQueueEntries)
	defer a.failovers.history.Close()
	projects.Decisions = a.failovers.history
	a.votes = &voteBatcher{a: a}
	projects.ManagedRestart = a.managed
	projects.FastSnapshot = a.managed

	// 容器有变化时上报状态；容器重建后要重新接入项目内网（见 project/network.go）。
	netSync := make(chan struct{}, 1)
	go dc.WatchProjects(ctx, project.LabelProject, func() {
		a.intranet.forgetEndpoints() // 容器启动、停止、重建：缓存的容器地址可能不对了
		a.changed()
		a.egress.trigger()
		select {
		case netSync <- struct{}{}:
		default:
		}
	}, a.intranet.endpointEvents)
	go func() {
		projects.SyncAllNetworks(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-netSync:
				time.Sleep(time.Second) // 一次部署会连续产生很多事件，攒一下再处理
				projects.SyncAllNetworks(ctx)
				a.intranet.trigger()
			}
		}
	}()
	a.metrics = metrics.New(metricDB, dc, cfg.DataDir, log.With("component", "metrics"))
	if err := a.metrics.UseBudget(ctx, cfg.Logs, filepath.Join(cfg.DataDir, "logs", "metrics.db"), metricDB != nodeDB); err != nil {
		log.Error("无法把监控历史接入统一本机容量", "err", err)
	}
	if cfg.Logs != nil {
		a.metrics.OnSample = logging.CaptureMetrics(cfg.Logs, cfg.LogConfig)
	}
	if cfg.LogConfig.NodeDetails {
		a.metrics.AddSource(metrics.NodeDetails())
	}
	a.metrics.ExtraRestarts = projects.ManagedRestartCount
	a.metrics.ProjectDirs = func() []string { return projects.Dirs(context.Background()) }
	hintDirs = func() []string { return append([]string{cfg.DataDir}, projects.Dirs(context.Background())...) }
	if !cfg.DisableProxy {
		proxy.OnAccess(a.logProxyAccess)
		defer proxy.OnAccess(nil)
		proxy.OnConnection(a.logProxyConnection)
		defer proxy.OnConnection(nil)
		a.metrics.AddSource(routeMetrics())
	}
	a.metrics.AddSource(a.meshMetrics())
	statistics := logging.Statistics{Store: cfg.Logs}
	if metricDB != nodeDB {
		statistics.History = a.metrics
	}
	a.alerts = alerts.New(log.With("component", "alerts"), hostname, filepath.Join(cfg.DataDir, "identity"),
		filepath.Join(cfg.DataDir, "caddy"), statistics, dc,
		func(id string) string { return projects.ProjectName(context.Background(), id) }, a.relayAlert)
	a.alerts.Report = a.reportAlert
	a.alerts.ContainerRules = a.containerAlertRules
	a.alerts.RouteRules = a.routeAlertRules
	a.alerts.Domains = func() map[string]bool {
		routes, err := a.allRoutes(context.Background())
		if err != nil {
			return nil
		}
		out := map[string]bool{}
		for _, r := range append(routes, a.panelRoutes()...) {
			for _, d := range r.Domains {
				out[d] = true
			}
		}
		return out
	}
	go a.alerts.Run(ctx)
	if err := a.replicas.usable(); err != nil {
		msg := status.Convert(err).Message()
		log.Error("副本位置记录无法读取", "file", a.replicas.file, "err", a.replicas.brokenCause, "detail", msg)
		go a.alerts.Fire(ctx, "replica-index", "副本位置记录无法读取", msg, "")
	}
	if err := a.startMesh(ctx, cfg.MeshPort); err != nil {
		log.Warn("mesh 启动失败，依赖 mesh 的功能不可用", "err", err)
	}
	if cfg.Logs != nil {
		captureMesh := logging.CaptureMesh(cfg.Logs, cfg.LogConfig)
		a.metrics.AddSource(func(float64) map[string]float64 {
			if a.mesh == nil {
				captureMesh(time.Now(), nil)
			} else {
				captureMesh(time.Now(), mesh.StatusProto(a.mesh.m, a.mesh.name))
			}
			return nil
		})
	}
	go a.metrics.Run(ctx)
	copyCtx, stopCopies := context.WithCancel(ctx)
	copiesDone := make(chan struct{})
	go func() { defer close(copiesDone); a.logCopiesLoop(copyCtx) }()
	defer func() { stopCopies(); <-copiesDone }()
	go a.intranet.run(ctx)
	go a.egress.run(ctx)
	// 任何容器（包括不是 Nagi 管理的）接入网络时都重新生成出站规则：拦截模式的网桥上，地址不在规则里的连接会被丢弃，
	// 同一个网桥上新启动的其他容器不能等 30 秒的定时同步（见 docs/agent.md#出站限制）。
	go dc.WatchNetworkConnects(ctx, func() {
		a.intranet.forgetEndpoints() // 容器接入网络（分到了地址）
		a.egress.trigger()
	}, a.intranet.endpointEvents)
	go a.violations.run(ctx)
	for _, id := range a.failovers.ids() {
		a.startFailover(ctx, id)
	}
	proxy.OnCertEvent(func(domain string, err error) {
		data := map[string]any{"domain": domain, "result": "obtained"}
		if err != nil {
			data["result"], data["error"] = "failed", err.Error()
		}
		cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "proxy.certificate", Object: domain, Data: data})
		if a.direct.certEvent(domain, err) {
			go a.changed() // 直连下载的 IP 证书：只更新状态，不告警
			return
		}
		a.certFails.set(domain, err)
		go a.changed()
		// Caddy 在申请证书的过程中同步调用，发送告警放到后台。
		if err == nil {
			go a.alerts.Resolve(ctx, "cert_failed:"+domain)
			return
		}
		if !a.alerts.NodeEnabled(alert.RuleCert) {
			return
		}
		go a.alerts.Fire(ctx, "cert_failed:"+domain, fmt.Sprintf("域名 %s 申请证书失败", domain), err.Error(),
			a.domainProject(ctx, domain))
	})
	go a.snapshotLoop(ctx)
	go a.backupProgressLoop(ctx)
	go a.resumeInterruptedSyncs(ctx)
	go a.imageCheckLoop(ctx)

	if !cfg.DisableProxy {
		a.proxy = proxy.New(filepath.Join(cfg.DataDir, "caddy"), log.With("component", "proxy"))
		proxy.SetDirectHandler(a.direct)
		a.direct.reload = a.applyProxy
		a.proxyKick = make(chan struct{}, 1)
		defer a.proxy.Stop()
		go a.proxyLoop(ctx)
	}

	cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "node.ready", Data: map[string]any{"version": cfg.Version, "hostname": hostname, "embedded": cfg.Embedded}})
	a.superviseLoop(ctx)
	return nil
}

type agent struct {
	cfg      Config
	log      *slog.Logger
	id       *identity
	docker   *docker.Client
	projects *project.Manager
	files    *nodefiles.Service
	hostname string
	info     *nagiv1.NodeInfo
	upgrade  upgradeState

	proxy     *proxy.Proxy
	proxyKick chan struct{}
	// proxyMu：applyProxy 同时只有一个在做。
	proxyMu   sync.Mutex
	certFails certFailures
	metrics   *metrics.Collector
	alerts    *alerts.Engine
	mesh      *meshNode
	intranet  *intranet
	direct    *directDL // 直连下载（见 directdl.go）
	// egress 是容器出站限制（见 docs/agent.md#出站限制）。
	egress *egress
	// violations 收集被拒绝的越权请求，上报给面板（见 docs/mesh.md#越权识别）。
	violations *violations
	// audit 是节点本地审计，面板用 ReadAuditLog 读走后代为外送（见 audit.go）。
	audit      *auditlog.Log
	recordings *recording.Recordings
	replicas   *replicas
	backups    *backupStates
	failovers  *failovers
	votes      *voteBatcher
	ctx        context.Context
	startedAt  time.Time
	// confirmUpgrade 在连上面板后调用：刚升级时表示新版本能用（见 upgrade_guard.go）。
	confirmUpgrade func()

	mu       sync.Mutex
	subs     []chan struct{}
	panels   map[string]nagiv1.PanelServiceClient // 面板指纹 → 当前连接，用于请面板代发告警
	sessions map[string]context.CancelFunc        // 面板指纹 → 停止连接
	connErrs map[string]string                    // 面板指纹 → 最近一次连接失败的原因
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
	if pruned, err := a.id.pruneClusters(); err != nil {
		a.log.Warn("清理备用面板失败", "err", err)
	} else if pruned {
		a.log.Info("主面板已不在信任列表里，移除了它的备用面板")
	}
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
	alertPanels := make([]string, 0, len(want))
	for fp, p := range want {
		keep = append(keep, fp)
		// 备用面板的告警配置不用（它当主面板时下发过的，现在以主面板的为准），否则渠道会重复、阈值取了旧的。
		if a.role(p) != "standby" {
			alertPanels = append(alertPanels, fp)
		}
	}
	a.alerts.Retain(alertPanels)
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

// reportAlert 把发出的告警告诉所有连着的面板。旧版面板没有这个接口，失败就算了。
func (a *agent) reportAlert(ctx context.Context, al *nagiv1.Alert) {
	a.mu.Lock()
	list := make([]nagiv1.PanelServiceClient, 0, len(a.panels))
	for _, c := range a.panels {
		list = append(list, c)
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, c := range list {
		_, _ = c.ReportAlert(ctx, &nagiv1.ReportAlertRequest{Alert: al})
	}
}

// proxyLoop 在项目或容器变化时重新生成 Caddy 配置；另外每 15 秒检查一次，及时发现新拿到的证书。
func (a *agent) proxyLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		a.applyProxy(ctx)
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

// applyProxy 重新生成并加载 Caddy 配置。proxyLoop 定时调用；加密 HTTP 下载签发命令时也立即调用一次，
// 好在回答面板之前知道端口开没开好（见 directDL.startHTTP）。同时只有一个在做。
func (a *agent) applyProxy(ctx context.Context) {
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	routes, err := a.allRoutes(ctx)
	routes = append(routes, a.panelRoutes()...)
	https := a.direct.wanted(a.proxy)
	a.proxy.SetDirect(https)
	a.proxy.SetDirectHTTP(a.direct.wantedHTTP(a.proxy, https))
	if err == nil {
		err = a.proxy.Apply(routes)
	}
	data := map[string]any{"result": "ok", "rules": len(routes)}
	if err != nil {
		data["result"], data["error"] = "failed", err.Error()
	}
	a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "proxy.apply", Data: data})
	if err == nil {
		// 域名移除不是证书恢复的证据，保留未确认恢复的记录。
		domains := map[string]bool{}
		for _, r := range routes {
			for _, d := range r.Domains {
				domains[d] = true
			}
		}
		a.alerts.MarkUnavailableUnless("cert_failed:", func(d string) bool { return domains[d] })
	}
	if err != nil && ctx.Err() == nil {
		a.log.Warn("更新对外访问规则失败", "err", err)
	}
}

// changed 通知所有面板连接：容器状态有变化，需要重新上报。
func (a *agent) changed() {
	if a.proxyKick != nil {
		select {
		case a.proxyKick <- struct{}{}:
		default:
		}
	}
	a.notifyReport()
}

// notifyReport 让各个面板连接马上上报一次（不重新生成 Caddy 配置），例如同步进度变了。
func (a *agent) notifyReport() {
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
		Hostname:     a.hostname,
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Version:      a.cfg.Version,
		StartedAt:    a.startedAt.Unix(),
		Embedded:     a.cfg.Embedded,
		ProjectsRoot: a.projectsRoot(),
		Cpus:         int32(runtime.NumCPU()),

		CrossNodeEntry:  true,
		ProxyProtocol:   true,
		MultiBackup:     true,
		AsyncBackupSync: true,
		FileOps:         true,
		ImageOps:        true,
		ReplicaDirs:     true,
		ProxyDisabled:   a.cfg.DisableProxy,
		EgressError:     a.egress.Error(),
		ReportsCerts:    true,

		NodeAccess:         nodeAccessSupported() && !NodeAccessDisabled(a.cfg.DataDir),
		NodeAccessDisabled: NodeAccessDisabled(a.cfg.DataDir),
		InContainer:        inContainer(),
	}
	info.MemoryBytes, info.DiskBytes = metrics.Capacity(a.cfg.DataDir)
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

// connectLoop 保持与一个面板的连接，断开后按指数退避重连（最长 10 秒）。每次连接都读取信任列表里最新的条目
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
		data := map[string]any{"duration_ms": time.Since(start).Milliseconds(), "context_canceled": ctx.Err() != nil}
		if err != nil {
			data["error"] = err.Error()
		}
		a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "panel.connection.ended", Object: fingerprint, Data: data})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.setConnectError(fingerprint, err)
		a.log.Warn("与面板的连接已断开，稍后重连", "panel", p.Address, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + time.Duration(mrand.Int64N(int64(backoff/5)+1))):
		}
		// 上限不能太长：面板重启以后，节点要等到下一次重连才回来，这期间界面上所有项目都显示节点离线。
		// 加上最多 20% 的随机等待，很多节点不会在同一刻一起连。
		backoff = min(backoff*2, 10*time.Second)
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

	guardUnary, guardStream := a.primaryGuard(p.Fingerprint)
	auditUnary, auditStream := a.auditInterceptors(p.Fingerprint)
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(explainUnary, auditUnary, guardUnary, detachUnary), grpc.ChainStreamInterceptor(explainStream, auditStream, guardStream, detachStream),
		grpc.MaxRecvMsgSize(64<<20))
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
	a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "panel.connection.established", Object: p.Fingerprint, Data: map[string]any{"panel": p.Address}})
	a.confirmUpgrade()
	a.setConnectError(p.Fingerprint, nil)
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
		resp, err := a.report(ctx, panel, p)
		if err != nil {
			return err
		}
		switched := a.applyCluster(p, resp.GetCluster())
		if key, ok := a.mesh.update(p.Fingerprint, p.Address, resp.GetMesh()); ok && !relaying {
			relaying = true
			go a.mesh.relayLoop(sctx, panel, key)
		}
		if switched {
			continue // 马上告诉面板节点已经认它是主面板
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

// liveInfo 是上报用的节点信息：启动时算好的（a.info）加上随时会变的部分。
func (a *agent) liveInfo() *nagiv1.NodeInfo {
	info := proto.Clone(a.info).(*nagiv1.NodeInfo)
	info.ProjectsRoot = a.projectsRoot()
	_, info.ReplicaDir = a.replicas.defaultDir()
	// 本地命令随时可能改这个开关，界面要跟着变。
	info.NodeAccess = nodeAccessSupported() && !NodeAccessDisabled(a.cfg.DataDir)
	info.NodeAccessDisabled = NodeAccessDisabled(a.cfg.DataDir)
	// 证书签好、开关切换、地址变了都会变。a.info 算出来时 a.direct 还没有创建。
	info.DirectDownload = a.direct.status(a.proxy)
	return info
}

func (a *agent) report(ctx context.Context, panel nagiv1.PanelServiceClient, p trustedPanel) (*nagiv1.ReportResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	containers := a.projects.Statuses(ctx)
	for _, c := range containers {
		if c.Error != "" {
			c.Error = explain(c.Error)
		}
	}
	info := a.liveInfo()
	latest := a.metrics.Latest()
	usage := &nagiv1.NodeUsage{Cpu: latest["node:cpu"], Memory: latest["node:mem"], Disk: latest["node:disk"]}
	for _, d := range a.metrics.Disks() {
		usage.Disks = append(usage.Disks, &nagiv1.DiskUsage{Mount: d.Mount, TotalBytes: d.Total, UsedBytes: d.Used})
	}
	st := a.clusterOf(p)
	var primaryAddr string
	if pp, ok := a.id.panel(st.Primary); ok {
		primaryAddr = pp.Address
	}
	return panel.Report(ctx, &nagiv1.ReportRequest{Node: info, Containers: containers, Mesh: a.mesh.info(), Failovers: a.failoverStatuses(ctx), Usage: usage,
		Term: st.Term, Primary: st.Primary, PrimaryAddress: primaryAddr, Panels: trustedFingerprints(a.id.Panels()), Certs: a.certStatuses(ctx)})
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
	routes, _ := a.allRoutes(ctx)
	for _, r := range routes {
		if slices.Contains(r.Domains, domain) {
			return r.Project
		}
	}
	return ""
}

// routeMetrics 把 Caddy 统计的请求和连接转成监控序列。有过请求（tcp/udp 是连接）的规则，之后没有时记 0，
// 超过一天没有就不再记录（规则可能已经删除）。
func routeMetrics() metrics.Source {
	type seen struct {
		at       time.Time
		l4       bool
		statuses map[int]bool
	}
	lastActive := map[string]seen{}
	return func(elapsed float64) map[string]float64 {
		now := time.Now()
		stats := proxy.TakeStats()
		for k, s := range stats {
			if s.Requests > 0 || s.Conns > 0 || s.Active > 0 {
				t := lastActive[k]
				t.at, t.l4 = now, s.L4
				if t.statuses == nil {
					t.statuses = map[int]bool{}
				}
				for status := range s.Statuses {
					t.statuses[status] = true
				}
				lastActive[k] = t
			}
		}
		out := map[string]float64{}
		for k, t := range lastActive {
			if now.Sub(t.at) > 24*time.Hour {
				delete(lastActive, k)
				continue
			}
			s := stats[k]
			prefix := "r:" + k + ":"
			// tcp/udp：新连接、连不上容器的次数、收发字节是每秒速率，active 是当前连接数（见 docs/monitoring.md#tcp--udp-统计）。
			if t.l4 {
				out[prefix+"conn"] = float64(s.Conns) / elapsed
				out[prefix+"fail"] = float64(s.Fails) / elapsed
				out[prefix+"active"] = float64(s.Active)
				out[prefix+"in"] = float64(s.BytesIn) / elapsed
				out[prefix+"out"] = float64(s.BytesOut) / elapsed
				continue
			}
			out[prefix+"req"] = float64(s.Requests) / elapsed
			out[prefix+"interval_seconds"] = elapsed
			out[prefix+"request_count"] = float64(s.Requests)
			out[prefix+"timed_count"] = float64(s.Timed)
			out[prefix+"latency_samples"] = float64(len(s.Durations))
			for status := range t.statuses {
				out[prefix+"status@"+strconv.Itoa(status)] = float64(s.Statuses[status])
			}
			out[prefix+"2xx"] = float64(s.Classes[2]) / elapsed
			out[prefix+"3xx"] = float64(s.Classes[3]) / elapsed
			out[prefix+"4xx"] = float64(s.Classes[4]) / elapsed
			out[prefix+"5xx"] = float64(s.Errors) / elapsed
			out[prefix+"in"] = float64(s.BytesIn) / elapsed
			out[prefix+"out"] = float64(s.BytesOut) / elapsed
			// 只有 WebSocket 的时段没有耗时，和没有请求一样不报。
			if s.Timed > 0 {
				out[prefix+"lat"] = 1000 * s.Seconds / float64(s.Timed)
				out[prefix+"max_latency"] = 1000 * s.Maximum
				out[prefix+"p50"] = 1000 * s.Percentile(0.50)
				out[prefix+"p95"] = 1000 * s.Percentile(0.95)
				out[prefix+"p99"] = 1000 * s.Percentile(0.99)
			}
		}
		return out
	}
}

// snapshotLoop 每分钟检查一次，给到时间的容器做定时快照。
func (a *agent) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.projects.SnapshotDue(ctx, func(projectID, container, task string, err error) {
			if task == "maintain" {
				key := "maintain:" + projectID + "/" + container
				if err == nil {
					a.alerts.Resolve(ctx, key)
					return
				}
				a.log.Warn("快照仓库维护失败", "project", projectID, "container", container, "err", err)
				if !a.alerts.ContainerEnabled(projectID, container, alert.RuleSnapshot) {
					return
				}
				a.alerts.Fire(ctx, key, "容器 "+container+" 快照仓库维护失败", err.Error(), projectID)
				return
			}
			key := "snapshot:" + projectID + "/" + container
			if err == nil {
				a.alerts.Resolve(ctx, key)
				// 每次定时快照成功后同步到备份节点或热备（没有设置时什么都不做）。
				_ = a.startBackupSync(ctx, projectID, container, "")
				go a.syncStandby(ctx, projectID+"-"+container)
				return
			}
			a.log.Warn("定时快照失败", "project", projectID, "container", container, "err", err)
			if !a.alerts.ContainerEnabled(projectID, container, alert.RuleSnapshot) {
				return
			}
			a.alerts.Fire(ctx, key, "容器 "+container+" 定时快照失败", err.Error(), projectID)
		})
	}
}

// projectsRoot 返回项目根目录：容器目录默认是 <项目根目录>/<网络名>/<容器名>。保存在 identity/projects-root，默认 /srv/nagi。
func (a *agent) projectsRoot() string {
	b, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "identity", "projects-root"))
	if err != nil {
		return project.DefaultProjectsRoot
	}
	if r := strings.TrimSpace(string(b)); r != "" {
		return r
	}
	return project.DefaultProjectsRoot
}

func (a *agent) setProjectsRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" || !strings.HasPrefix(root, "/") || filepath.Clean(root) == "/" {
		return project.ErrInvalidArgument
	}
	// 系统目录不能当项目根目录：节点以 root 在根目录下建目录、写 compose 和数据。
	clean := filepath.Clean(root)
	for _, sys := range systemDirs {
		if clean == sys || strings.HasPrefix(clean, sys+"/") {
			return project.ErrInvalidArgument
		}
	}
	return os.WriteFile(filepath.Join(a.cfg.DataDir, "identity", "projects-root"), []byte(filepath.Clean(root)+"\n"), 0o644)
}

// systemDirs 是不能当项目根目录的系统目录（包括它们下面的目录）。
var systemDirs = []string{"/etc", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/usr", "/boot", "/proc", "/sys", "/dev", "/run"}

func trustedFingerprints(panels []trustedPanel) []string {
	out := make([]string, 0, len(panels))
	for _, p := range panels {
		out = append(out, p.Fingerprint)
	}
	return out
}

// containerAlertRules 返回容器生效的告警规则（面板随容器配置下发）。本节点上没有这个容器，或者面板是旧版本、没有下发时 ok 为 false。
func (a *agent) containerAlertRules(projectID, container string) (*nagiv1.AlertRules, bool) {
	c, err := a.projects.Get(context.Background(), projectID, container)
	if err != nil || c.Spec.GetAlertRules() == nil {
		return nil, false
	}
	return c.Spec.GetAlertRules(), true
}

// routeAlertRules 返回 http 规则（映射）所属容器的告警规则：容器在本节点上时看它的配置，
// 本节点只是入口节点时看入口计划里带的。
func (a *agent) routeAlertRules(projectID, route string) (*nagiv1.AlertRules, bool) {
	for _, c := range a.projects.List(context.Background()) {
		if c.Spec.GetProjectId() != projectID {
			continue
		}
		for _, r := range c.Spec.GetRoutes() {
			if r.GetName() == route && c.Spec.GetAlertRules() != nil {
				return c.Spec.GetAlertRules(), true
			}
		}
	}
	if a.intranet != nil {
		for _, e := range a.intranet.current().GetEntries() {
			if e.GetProjectId() == projectID && e.GetName() == route && e.GetAlertRules() != nil {
				return e.GetAlertRules(), true
			}
		}
	}
	return nil, false
}
