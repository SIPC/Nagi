// Package server 是面板：Web UI、Connect 接口、节点控制连接都在同一个面板端口上。
package server

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/pkg/tunnel"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/api"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"github.com/SIPC/nagi/web"
)

// Config 是面板的配置。
type Config struct {
	// DataDir 是面板的数据目录。
	DataDir string
	// Port 是面板端口；为 0 时首次启动随机选一个并记住。
	Port   int
	Logger *slog.Logger
	// Version 是面板（也是它提供给节点的 nagi）的版本。
	Version string
	// SelfTLS：本机没有 Caddy（--no-agent 或 --no-proxy），绑定域名时由面板自己申请证书、监听 80/443。
	SelfTLS bool
	// NewMasterKey：用户确认主密钥丢了（--new-master-key）。见 checkMasterKey。
	NewMasterKey bool
	// StandbyCode：作为备用面板启动时主面板生成的配对码（只在第一次启动、数据目录为空时用）。
	StandbyCode string
}

const (
	settingPort    = "port"
	enrollTokenTTL = time.Hour
)

// Server 是面板。New 之后端口已经监听，可以先取 Addr、Fingerprint 再调用 Run。
type Server struct {
	cfg  Config
	log  *slog.Logger
	db   *sql.DB
	q    *store.Queries
	ca   *tlsutil.CA
	cert tls.Certificate

	fingerprint   string
	port          int
	publicAddress string
	listener      net.Listener

	auth     *auth.Service
	nodes    *nodes.Registry
	projects *projects.Service
	sealer   *sealer.Sealer
	domainCh chan string

	// 主备面板（见 docs/standby.md）：本面板是主面板时 primary 给备用面板发数据；是备用面板时 standby 不为空。
	primary *standby.Primary
	standby *standby.Client
	role    role
}

// IsStandby 判断本面板是不是备用面板。
func (s *Server) IsStandby() bool { return s.standby != nil }

// checkMasterKey 在主密钥文件不见了、数据库里却有用它加密的数据时拒绝启动：悄悄生成新密钥，
// 这些数据就再也解不开了，而用户多半只是忘了把备份的密钥放回来。
// 用户确认密钥确实丢了（NewMasterKey）时清掉解不开的数据，接着生成新密钥。
func (s *Server) checkMasterKey(ctx context.Context, path string) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	n, err := s.q.CountSealedData(ctx)
	if err != nil || n == 0 {
		return err
	}
	if !s.cfg.NewMasterKey {
		return fmt.Errorf("找不到面板主密钥 %s，但数据库里有用它加密的数据（两步验证、告警配置、项目数据）。"+
			"请把备份的主密钥文件放回这个位置再启动。确实丢了的话，加 --new-master-key 启动：生成新的主密钥，"+
			"关闭两步验证，清掉告警配置（要重新填写，在那之前节点继续按原来的配置发告警）和项目数据（从节点上缓存的配置恢复）", path)
	}
	if err := s.q.ClearTOTPSecrets(ctx); err != nil {
		return err
	}
	if err := nodes.MarkAlertConfigLost(ctx, s.q); err != nil {
		return err
	}
	// 项目数据用旧密钥加密，解不开了；新面板从节点上缓存的配置把项目拼回来（见 docs/panel.md#项目数据）。
	if list, err := s.q.ListProjects(ctx); err == nil {
		for _, p := range list {
			if err := s.q.DeleteProject(ctx, p.ID); err != nil {
				return err
			}
		}
	}
	s.log.Warn("主密钥丢失：已生成新的主密钥，关闭了两步验证，清掉了告警配置和项目数据", "path", path)
	return nil
}

// New 打开数据库、准备证书、监听面板端口。
func New(ctx context.Context, cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	d, err := db.OpenMigrated(ctx, filepath.Join(cfg.DataDir, "panel.db"), store.Migrations)
	if err != nil {
		return nil, fmt.Errorf("打开面板数据库: %w", err)
	}
	s := &Server{cfg: cfg, log: log, db: d, q: store.New(d), domainCh: make(chan string, 1), role: role{ch: make(chan struct{})}}

	s.publicAddress, err = s.setting(ctx, api.SettingPublicAddress, detectAddress)
	if err != nil {
		return nil, err
	}
	// 自带节点的 Caddy 反代面板时带的请求头的值：面板据此才相信 X-Forwarded-For（见 auth.RequestIP）。
	proxySecret, err := s.setting(ctx, nodes.SettingProxySecret, func() string {
		t, _ := auth.RandomToken()
		return t
	})
	if err != nil {
		return nil, err
	}
	auth.SetProxySecret(proxySecret)
	if err := s.listen(ctx); err != nil {
		d.Close()
		return nil, err
	}
	// 第一次以备用面板启动：先向主面板配对，拿到 CA 和主密钥。
	info, err := standby.Load(cfg.DataDir)
	if err != nil {
		d.Close()
		return nil, err
	}
	// 配过对的数据目录（之后可能提升成了主面板）不再配对。
	if info == nil && cfg.StandbyCode != "" && !standby.Paired(cfg.DataDir) {
		host, _ := os.Hostname()
		if err := standby.Pair(ctx, cfg.DataDir, cfg.StandbyCode, host, net.JoinHostPort(s.publicAddress, strconv.Itoa(s.port))); err != nil {
			d.Close()
			return nil, fmt.Errorf("与主面板配对: %w", err)
		}
		if info, err = standby.Load(cfg.DataDir); err != nil {
			d.Close()
			return nil, err
		}
		log.Info("已与主面板配对，本面板是备用面板", "primary", info.PrimaryAddress)
	}
	if err := s.loadPKI(); err != nil {
		d.Close()
		return nil, fmt.Errorf("准备证书: %w", err)
	}
	keyPath := filepath.Join(cfg.DataDir, "pki", "master.key")
	if err := s.checkMasterKey(ctx, keyPath); err != nil {
		d.Close()
		return nil, err
	}
	s.sealer, err = sealer.Load(keyPath)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("读取面板主密钥: %w", err)
	}
	s.auth = auth.New(s.q, s.sealer)
	if info != nil {
		s.standby = &standby.Client{Info: info, DataDir: cfg.DataDir, DB: d, Log: log.With("component", "standby")}
	} else {
		s.primary = &standby.Primary{Q: s.q, DataDir: cfg.DataDir, CA: s.ca, Log: log.With("component", "standby")}
	}
	s.nodes = nodes.NewRegistry(s.q, log, s.sealer, s.ca)
	s.nodes.SetWitnessDir(filepath.Join(cfg.DataDir, "witness"))
	s.projects = &projects.Service{Q: s.q, Sealer: s.sealer, Nodes: s.nodes, Log: log, Fingerprint: s.Fingerprint,
		ReservedDomain: func(d string) bool {
			v, err := s.q.GetSetting(context.Background(), api.SettingDomain)
			return err == nil && v != "" && v == d
		}}
	s.nodes.Behind = s.behind
	s.nodes.Self = s.fingerprint
	if s.standby != nil {
		// 备用面板只查询：不给节点补推配置、不下发告警配置，不进 mesh（见 docs/standby.md）。
		s.nodes.ReadOnly = true
		return s, nil
	}
	// 主备信息只由主面板告诉节点：备用面板（包括没提升成的）不发，节点不会因为它改认主面板。
	s.nodes.Cluster = func(ctx context.Context) *nagiv1.PanelCluster {
		addr := s.publicAddress
		if v, err := s.q.GetSetting(ctx, api.SettingPublicAddress); err == nil && v != "" {
			addr = v
		}
		return standby.Cluster(ctx, s.q, net.JoinHostPort(addr, strconv.Itoa(s.port)))
	}
	if err := standby.Init(ctx, s.q, s.fingerprint); err != nil {
		d.Close()
		return nil, err
	}
	// 记下本面板（主面板）现在的地址，随同步传给备用面板。
	if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingPrimaryAddress, Value: s.hostPort(ctx)}); err != nil {
		d.Close()
		return nil, err
	}
	s.nodes.OnReport = s.projects.Sync
	if m, err := s.startMesh(); err != nil {
		log.Warn("mesh 启动失败，依赖 mesh 的功能不可用", "err", err)
	} else {
		s.nodes.SetMesh(m, "面板 "+s.publicAddress)
	}
	return s, nil
}

// startMesh 启动面板的 mesh：UDP 用和面板端口相同的号码，同时替节点中转。
func (s *Server) startMesh() (*mesh.Mesh, error) {
	path := filepath.Join(s.cfg.DataDir, "pki", "mesh.key")
	var priv mesh.Key
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := crand.Read(priv[:]); err != nil {
			return nil, err
		}
		// WireGuard 私钥的标准钳位处理
		priv[0] &= 248
		priv[31] = (priv[31] & 127) | 64
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv[:])), 0o600); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(raw) != len(priv) {
			return nil, errors.New("mesh 私钥文件损坏")
		}
		copy(priv[:], raw)
	}
	m, err := mesh.New(mesh.Config{PrivateKey: priv, Port: uint16(s.port), Relay: true, Logger: s.log.With("component", "mesh")})
	if err != nil {
		return nil, err
	}
	s.log.Info("mesh 已启动", "address", m.Addr(), "udp_port", m.Port())
	return m, nil
}

// Port 返回面板端口。
func (s *Server) Port() int { return s.port }

// LocalAddr 返回本机访问面板的地址，供内嵌的 agent 使用。
func (s *Server) LocalAddr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)) }

// URL 返回面板的对外访问地址。
func (s *Server) URL() string {
	if d, err := s.q.GetSetting(context.Background(), api.SettingDomain); err == nil && d != "" {
		return "https://" + d
	}
	addr := s.publicAddress
	if v, err := s.q.GetSetting(context.Background(), api.SettingPublicAddress); err == nil && v != "" {
		addr = v
	}
	return "https://" + net.JoinHostPort(addr, strconv.Itoa(s.port))
}

// Fingerprint 返回面板公钥指纹。
func (s *Server) Fingerprint() string { return s.fingerprint }

// CreateEnrollToken 生成一个一次性的节点加入令牌，1 小时内有效。
func (s *Server) CreateEnrollToken(ctx context.Context) (string, error) {
	token, err := auth.RandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	err = s.q.CreateEnrollToken(ctx, store.CreateEnrollTokenParams{
		TokenHash: auth.HashToken(token), CreatedAt: now.Unix(), ExpiresAt: now.Add(enrollTokenTTL).Unix(),
	})
	return token, err
}

// Run 提供服务，直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	defer s.db.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.standby != nil {
		go s.standby.Run(ctx)
		go s.watchPrimary(ctx)
	} else if err := s.printSetupLink(ctx); err != nil {
		return err
	}
	go s.auth.CleanupLoop(ctx)

	mux := http.NewServeMux()
	a := &api.API{Q: s.q, Auth: s.auth, Nodes: s.nodes, Projects: s.projects, CA: s.ca, Sealer: s.sealer, Log: s.log, Panel: &api.PanelInfo{
		Q:           s.q,
		DataDir:     s.cfg.DataDir,
		Port:        s.port,
		Fingerprint: s.fingerprint,
		Version:     s.cfg.Version,
		OnDomainChange: func(d string) {
			select {
			case s.domainCh <- d:
			default:
			}
		},
		NewEnrollToken: func(ctx context.Context) (string, time.Time, error) {
			token, err := s.CreateEnrollToken(ctx)
			return token, time.Now().Add(enrollTokenTTL), err
		},
		Primary: s.primary,
		Standby: s.standby,
		Promote: s.Promote,
	}}
	a.Mount(mux)
	if s.primary != nil {
		mux.HandleFunc("/api/standby/pair", s.primary.HandlePair)
	}
	mux.HandleFunc("GET /api/standby/status", s.handleStatus)
	mux.Handle("/", web.Handler())

	httpLn := newChanListener(s.listener.Addr())
	srv := &http.Server{
		Handler:           securityHeaders(s.auth.EntranceMiddleware(mux), false),
		TLSConfig:         s.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	go s.acceptLoop(ctx, httpLn)
	// 备用面板不发告警、不当 mesh 中转（节点还没连它，见 docs/standby.md）。
	if s.standby == nil {
		go s.nodes.RunOfflineAlerts(ctx)
		go s.nodes.ServeMesh(ctx)
	}
	domainDone := make(chan struct{})
	go func() {
		s.serveDomain(ctx, s.auth.EntranceMiddleware(mux))
		close(domainDone)
	}()
	if m := s.nodes.Mesh(); m != nil {
		defer m.Close()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(httpLn) }()
	s.log.Info("面板已启动", "url", s.URL(), "fingerprint", s.fingerprint)
	if e := s.auth.Entrance(ctx); e != "" {
		s.log.Info("安全入口已开启，先打开这个地址才能看到登录页", "url", s.URL()+"/"+e)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.listener.Close()
		return srv.Shutdown(shutdownCtx)
	case <-s.role.ch:
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		s.listener.Close()
		_ = srv.Shutdown(shutdownCtx)
		cancel()
		// 等面板域名的 80/443 关掉，按新角色启动时才能再监听。
		select {
		case <-domainDone:
		case <-time.After(10 * time.Second):
		}
		return ErrRoleChanged
	case err := <-errc:
		return err
	}
}

// ---- 端口与连接分流 ----

func (s *Server) listen(ctx context.Context) error {
	port := s.cfg.Port
	if port == 0 {
		if v, err := s.q.GetSetting(ctx, settingPort); err == nil {
			port, _ = strconv.Atoi(v)
		}
	}
	if port != 0 {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			return fmt.Errorf("监听面板端口 %d: %w", port, err)
		}
		s.port, s.listener = port, ln
	} else {
		// 首次启动：随机选一个空闲的高位端口。
		for i := 0; i < 50; i++ {
			p := 20000 + rand.IntN(40000)
			ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
			if err == nil {
				s.port, s.listener = p, ln
				break
			}
		}
		if s.listener == nil {
			return errors.New("找不到可用的面板端口，请用 --port 指定")
		}
	}
	return s.q.SetSetting(ctx, store.SetSettingParams{Key: settingPort, Value: strconv.Itoa(s.port)})
}

func (s *Server) tlsConfig() *tls.Config {
	browser := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{s.cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	// 节点的控制连接必须出示本面板签发的客户端证书。
	agent := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.cert},
		NextProtos:   []string{tunnel.ALPN},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    s.ca.Pool(),
	}
	// 备用面板的同步连接：同样要出示本面板 CA 签发的客户端证书（见 docs/standby.md）。
	standbyCfg := agent.Clone()
	standbyCfg.NextProtos = []string{standby.ALPN}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{s.cert},
		NextProtos:   []string{"h2", "http/1.1"},
		// 按客户端声明的 ALPN 选择配置：只向节点和备用面板索要客户端证书，浏览器不会弹出证书选择框。
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, p := range hello.SupportedProtos {
				if p == tunnel.ALPN {
					return agent, nil
				}
				if p == standby.ALPN {
					return standbyCfg, nil
				}
			}
			return browser, nil
		},
	}
}

// acceptLoop 接受 TLS 连接，按 ALPN 分流：节点的控制连接交给节点注册表，其余交给 HTTP 服务。
func (s *Server) acceptLoop(ctx context.Context, httpLn *chanListener) {
	cfg := s.tlsConfig()
	handshakes := make(chan struct{}, 256)
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			httpLn.Close()
			return
		}
		// 同时进行的 TLS 握手数有上限，防止大量不发数据的连接占满资源。
		select {
		case handshakes <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		go func() {
			conn := tls.Server(raw, cfg)
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.HandshakeContext(hctx)
			cancel()
			<-handshakes
			if err != nil {
				conn.Close()
				return
			}
			switch conn.ConnectionState().NegotiatedProtocol {
			case tunnel.ALPN:
				s.nodes.Handle(ctx, conn)
				return
			case standby.ALPN:
				if s.primary != nil {
					s.primary.Serve(ctx, conn)
				} else {
					conn.Close()
				}
				return
			}
			if !httpLn.put(conn) {
				conn.Close()
			}
		}()
	}
}

// chanListener 是一个由 acceptLoop 喂连接的 net.Listener，供 http.Server 使用。
type chanListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) put(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// contentSecurityPolicy 只允许加载面板自己的脚本和资源。样式允许内联：终端（xterm）会插入 <style>。
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// securityHeaders 加上安全相关的响应头。HSTS 只在确定用的是正式证书时加（domainTLS：面板自己申请证书的域名入口；
// 或者请求经本机 Caddy 按域名转发过来），自签证书加了 HSTS，浏览器就不让用户点"继续访问"了。
func securityHeaders(h http.Handler, domainTLS bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		if domainTLS || viaLocalProxy(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.ServeHTTP(w, r)
	})
}

// viaLocalProxy 判断请求是不是本机 Caddy 按面板域名转发过来的（来自回环地址、Host 是域名）。
func viaLocalProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	name := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		name = h
	}
	return name != "" && name != "localhost" && net.ParseIP(name) == nil
}

// ---- 证书 ----

// loadPKI 读取或生成面板 CA 和服务端私钥。服务端证书每次启动重新签发，私钥不变，
// 所以公钥指纹保持不变。
func (s *Server) loadPKI() error {
	dir := filepath.Join(s.cfg.DataDir, "pki")
	caCertPath, caKeyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	serverKeyPath := filepath.Join(dir, "server.key")

	caCert, err := tlsutil.ReadCert(caCertPath)
	if errors.Is(err, os.ErrNotExist) {
		ca, err := tlsutil.NewCA("Nagi Panel CA")
		if err != nil {
			return err
		}
		if err := tlsutil.WriteKey(caKeyPath, ca.Key); err != nil {
			return err
		}
		if err := tlsutil.WriteCert(caCertPath, ca.Cert); err != nil {
			return err
		}
		s.ca = ca
	} else if err != nil {
		return err
	} else {
		caKey, err := tlsutil.ReadKey(caKeyPath)
		if err != nil {
			return err
		}
		s.ca = &tlsutil.CA{Cert: caCert, Key: caKey}
	}

	serverKey, err := tlsutil.ReadKey(serverKeyPath)
	if errors.Is(err, os.ErrNotExist) {
		if serverKey, err = tlsutil.GenerateKey(); err != nil {
			return err
		}
		if err := tlsutil.WriteKey(serverKeyPath, serverKey); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	hosts := append([]string{"localhost", "127.0.0.1", "::1", s.publicAddress}, localAddresses()...)
	cert, err := s.ca.IssueServer(serverKey, hosts)
	if err != nil {
		return err
	}
	s.cert = tls.Certificate{Certificate: [][]byte{cert.Raw, s.ca.Cert.Raw}, PrivateKey: serverKey, Leaf: cert}
	s.fingerprint = tlsutil.Fingerprint(cert)
	return nil
}

// ---- 设置 ----

// setting 读取设置；不存在时用 def 生成默认值并保存。
func (s *Server) setting(ctx context.Context, key string, def func() string) (string, error) {
	v, err := s.q.GetSetting(ctx, key)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	v = def()
	return v, s.q.SetSetting(ctx, store.SetSettingParams{Key: key, Value: v})
}

// printSetupLink 在面板尚未初始化时生成一次性初始化令牌（24 小时内有效），并在日志里打印初始化链接。
func (s *Server) printSetupLink(ctx context.Context) error {
	ok, err := s.auth.HasUsers(ctx)
	if err != nil || ok {
		return err
	}
	token, err := auth.RandomToken()
	if err != nil {
		return err
	}
	// 令牌 24 小时内有效（过期后重启面板会生成新的），记在哈希后面。
	value := auth.HashToken(token) + ":" + strconv.FormatInt(time.Now().Add(api.SetupTokenTTL).Unix(), 10)
	if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: api.SettingSetupToken, Value: value}); err != nil {
		return err
	}
	link := s.URL() + "/setup?token=" + token
	s.log.Info("面板尚未初始化，请打开初始化链接设置管理员账号", "url", link)
	fmt.Fprintf(os.Stderr, "\n  面板尚未初始化。请在浏览器中打开下面的链接（24 小时内有效），设置管理员账号：\n\n    %s\n\n  面板使用自签证书，浏览器会提示不安全，确认公钥指纹后继续即可：\n    %s\n\n", link, s.fingerprint)
	return nil
}

// detectAddress 返回本机默认路由出口的 IPv4 地址，作为面板对外地址的默认值。
// 这里的 UDP "连接" 不会真正发出数据包。
func detectAddress() string {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// localAddresses 返回本机所有网卡上的 IP，写进服务端证书，方便用任意 IP 访问。
func localAddresses() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// hostPort 返回本面板的对外地址和端口（host:port）。
func (s *Server) hostPort(ctx context.Context) string {
	addr := s.publicAddress
	if v, err := s.q.GetSetting(ctx, api.SettingPublicAddress); err == nil && v != "" {
		addr = v
	}
	return net.JoinHostPort(addr, strconv.Itoa(s.port))
}
