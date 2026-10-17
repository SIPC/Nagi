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

func (a *agent) panelDomainFile() string {
	return filepath.Join(a.cfg.DataDir, "identity", "panel-domain")
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
	panels := a.id.Panels()
	if len(panels) == 0 {
		return nil
	}
	p := panels[0]
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
		Upstreams:   []string{net.JoinHostPort("127.0.0.1", port)},
		UpstreamTLS: &proxy.UpstreamTLS{CA: block.Bytes, ServerName: "localhost"},
	}}
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
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	a.log.Info("面板域名已更新", "domain", d)
	a.changed()
	return &nagiv1.SetPanelDomainResponse{}, nil
}
