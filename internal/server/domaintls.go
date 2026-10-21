package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/SIPC/nagi/internal/server/api"
	"github.com/caddyserver/certmagic"
)

// 纯面板（--no-agent，或者内嵌节点不启动 Caddy）绑定域名时，本机没有 Caddy，由面板自己申请证书，
// 在 443 上提供服务；80 用于 HTTP 验证，其余请求跳转到 HTTPS。节点的控制连接仍然走面板端口。
// 有内嵌节点（带 Caddy）时，由它的 Caddy 反代面板（见 agent/paneldomain.go），这里不做任何事。

func (s *Server) serveDomain(ctx context.Context, handler http.Handler) {
	if !s.cfg.SelfTLS {
		return
	}
	var stop func()
	apply := func(domain string) {
		if stop != nil {
			stop()
			stop = nil
		}
		if domain == "" {
			return
		}
		var err error
		if stop, err = s.startDomain(ctx, domain, handler); err != nil {
			s.log.Error("面板域名启动失败", "domain", domain, "err", err)
		}
	}
	d, _ := s.q.GetSetting(ctx, api.SettingDomain)
	apply(d)
	for {
		select {
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			return
		case d := <-s.domainCh:
			apply(d)
		}
	}
}

func (s *Server) startDomain(ctx context.Context, domain string, handler http.Handler) (func(), error) {
	var cfg *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil },
	})
	cfg = certmagic.New(cache, certmagic.Config{Storage: &certmagic.FileStorage{Path: filepath.Join(s.cfg.DataDir, "certificates")}})
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{CA: certmagic.LetsEncryptProductionCA, Agreed: true})
	cfg.Issuers = []certmagic.Issuer{issuer}
	if err := cfg.ManageAsync(ctx, []string{domain}); err != nil {
		cache.Stop()
		return nil, err
	}
	tlsCfg := cfg.TLSConfig()
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)
	tlsCfg.MinVersion = tls.VersionTLS12

	ln443, err := listenRetry(":443")
	if err != nil {
		cache.Stop()
		return nil, err
	}
	ln80, err := listenRetry(":80")
	if err != nil {
		ln443.Close()
		cache.Stop()
		return nil, err
	}
	https := &http.Server{Handler: securityHeaders(handler, true), ReadHeaderTimeout: 10 * time.Second}
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	plain := &http.Server{Handler: issuer.HTTPChallengeHandler(redirect), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := https.Serve(tls.NewListener(ln443, tlsCfg)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("面板域名的 HTTPS 服务停止", "err", err)
		}
	}()
	go func() {
		if err := plain.Serve(ln80); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("面板域名的 HTTP 服务停止", "err", err)
		}
	}()
	s.log.Info("面板域名已启用，正在申请证书", "domain", domain)
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = https.Shutdown(sctx)
		_ = plain.Shutdown(sctx)
		cache.Stop()
	}, nil
}

// listenRetry 监听端口，端口一时被占用时（例如面板换角色重启，上一个还没完全关掉）重试几次。
func listenRetry(addr string) (net.Listener, error) {
	var err error
	for i := 0; i < 5; i++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}
