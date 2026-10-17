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
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
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
	Version      string
	Logger       *slog.Logger
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
	panels := id.Panels()
	if len(panels) == 0 {
		return errors.New("节点尚未加入任何面板")
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

	projects, err := project.NewManager(nodeDB, dc, cfg.SubnetPool)
	if err != nil {
		return err
	}

	a := &agent{
		cfg:      cfg,
		log:      log,
		id:       id,
		docker:   dc,
		projects: projects,
		hostname: hostname,
	}
	a.info = a.nodeInfo(ctx)

	go dc.WatchProjects(ctx, project.LabelProject, a.changed)

	if !cfg.DisableProxy {
		a.proxy = proxy.New(filepath.Join(cfg.DataDir, "caddy"), log.With("component", "proxy"))
		a.proxyKick = make(chan struct{}, 1)
		defer a.proxy.Stop()
		go a.proxyLoop(ctx)
	}

	var wg sync.WaitGroup
	for _, p := range panels {
		wg.Add(1)
		go func(p trustedPanel) {
			defer wg.Done()
			a.connectLoop(ctx, p)
		}(p)
	}
	wg.Wait()
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

	mu   sync.Mutex
	subs []chan struct{}
}

// proxyLoop 在项目或容器变化时重新生成 Caddy 配置；另外每 15 秒检查一次，及时发现新拿到的证书。
func (a *agent) proxyLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		routes, err := a.projects.Routes(ctx)
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

// connectLoop 保持与一个面板的连接，断开后按指数退避重连。
func (a *agent) connectLoop(ctx context.Context, p trustedPanel) {
	backoff := time.Second
	for ctx.Err() == nil {
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
	nagiv1.RegisterAgentServiceServer(srv, &agentService{projects: a.projects, changed: a.changed})
	go tunnel.Serve(sess, srv)
	defer srv.Stop()

	cc, err := tunnel.Dial(sess)
	if err != nil {
		return err
	}
	defer cc.Close()
	panel := nagiv1.NewPanelServiceClient(cc)

	a.log.Info("已连接面板", "panel", p.Address)
	a.renewIfNeeded(ctx, panel, p)
	notify, unsubscribe := a.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.report(ctx, panel); err != nil {
			return err
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

func (a *agent) report(ctx context.Context, panel nagiv1.PanelServiceClient) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	projects, err := a.projects.List(ctx)
	if err != nil {
		a.log.Warn("读取项目列表失败", "err", err)
	}
	_, err = panel.Report(ctx, &nagiv1.ReportRequest{Node: a.info, Projects: projects})
	return err
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
