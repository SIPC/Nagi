package agent

import (
	"context"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/SIPC/nagi/internal/agent/proxy"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 面板绑定域名：面板自带的节点用自己的 Caddy 把这个域名反代到面板端口，证书由 Caddy 申请。
// Caddy 连面板时用面板的 CA 校验身份（面板的服务端证书由它签发，包含 localhost），不需要信任自签证书。
// 转发时带上面板给的 X-Nagi-Proxy 请求头，面板据此才相信 X-Forwarded-For 里的客户端地址。

func (a *agent) panelDomainFile() string {
	return filepath.Join(a.cfg.DataDir, "identity", "panel-domain")
}

func (a *agent) proxySecretFile() string {
	return filepath.Join(a.cfg.DataDir, "identity", "panel-proxy-secret")
}

func (a *agent) panelDomain() string {
	b, err := os.ReadFile(a.panelDomainFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// panelRoutes 返回反代面板的规则（只有面板自带的节点、绑定了域名时才有）。
func (a *agent) panelRoutes() []proxy.Route {
	domain := a.panelDomain()
	if !a.cfg.Embedded || domain == "" {
		return nil
	}
	// 本机的面板：自带的节点用 127.0.0.1 连它（信任列表里还可能有别的面板，例如主备面板、移交）。
	var p trustedPanel
	found := false
	for _, x := range a.id.Panels() {
		if host, _, err := net.SplitHostPort(x.Address); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				p, found = x, true
				break
			}
		}
	}
	if !found {
		return nil
	}
	block, _ := pem.Decode([]byte(p.CAPEM))
	if block == nil {
		return nil
	}
	_, port, err := net.SplitHostPort(p.Address)
	if err != nil {
		return nil
	}
	return []proxy.Route{{
		Project: "nagi-panel", Name: "panel", Service: "panel", Protocol: "http", Domains: []string{domain},
		Upstreams:      []string{net.JoinHostPort("127.0.0.1", port)},
		UpstreamTLS:    &proxy.UpstreamTLS{CA: block.Bytes, ServerName: "localhost"},
		RequestHeaders: a.proxyHeaders(),
	}}
}

// proxyHeaders 是反代面板时加的请求头（客户端自己带的同名请求头会被覆盖）。
func (a *agent) proxyHeaders() map[string]string {
	b, err := os.ReadFile(a.proxySecretFile())
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return nil
	}
	return map[string]string{"X-Nagi-Proxy": strings.TrimSpace(string(b))}
}

func (s *agentService) SetPanelDomain(_ context.Context, req *nagiv1.SetPanelDomainRequest) (*nagiv1.SetPanelDomainResponse, error) {
	a := s.node
	if !a.cfg.Embedded {
		return nil, status.Error(codes.FailedPrecondition, "只有面板自带的节点反代面板")
	}
	d := strings.ToLower(strings.TrimSpace(req.GetDomain()))
	var err error
	if d == "" {
		err = os.Remove(a.panelDomainFile())
		if os.IsNotExist(err) {
			err = nil
		}
	} else {
		err = os.WriteFile(a.panelDomainFile(), []byte(d), 0o600)
	}
	if err == nil && req.GetProxySecret() != "" {
		err = os.WriteFile(a.proxySecretFile(), []byte(req.GetProxySecret()), 0o600)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	a.log.Info("面板域名已更新", "domain", d)
	a.changed()
	return &nagiv1.SetPanelDomainResponse{}, nil
}
