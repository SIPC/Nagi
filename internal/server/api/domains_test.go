package api

import (
	"context"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestDNSState(t *testing.T) {
	ctx := context.Background()
	old := lookupHost
	defer func() { lookupHost = old }()
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		if host == "panel.example.com" {
			return []string{"203.0.113.9"}, nil
		}
		return nil, context.DeadlineExceeded
	}
	cases := []struct {
		name  string
		addrs []string
		node  string
		want  string
	}{
		{"解析到这台节点", []string{"203.0.113.5", "203.0.113.6"}, "203.0.113.6", "ok"},
		{"解析不到", nil, "203.0.113.6", "unresolved"},
		{"解析到别处（CDN）", []string{"198.51.100.1"}, "203.0.113.6", "mismatch"},
		{"节点地址不知道", []string{"198.51.100.1"}, "", "unknown"},
		{"节点是内网地址，对不上说明不了什么", []string{"198.51.100.1"}, "192.168.1.5", "unknown"},
		{"节点是内网地址，内网 DNS 对得上", []string{"192.168.1.5"}, "192.168.1.5", "ok"},
		{"面板对外地址是域名，解析后对得上", []string{"203.0.113.9"}, "panel.example.com", "ok"},
		{"面板对外地址是域名，对不上", []string{"198.51.100.1"}, "panel.example.com", "unknown"},
	}
	for _, c := range cases {
		if got := dnsState(ctx, c.addrs, c.node); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCertState(t *testing.T) {
	report := &nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{ReportsCerts: true}, Certs: []*nagiv1.DomainCert{
		{Domain: "a.example.com", State: "failed", Error: "boom"},
	}}
	get := func(r *nagiv1.ReportRequest, domain string) *nagiv1.DomainEntryCheck {
		c := &nagiv1.DomainEntryCheck{}
		certState(c, r, domain)
		return c
	}
	if c := get(report, "a.example.com"); c.GetCert() != "failed" || c.GetCertError() != "boom" {
		t.Errorf("失败的证书: %+v", c)
	}
	if c := get(report, "new.example.com"); c.GetCert() != "pending" {
		t.Errorf("节点还没报的域名应该算申请中: %+v", c)
	}
	if c := get(&nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{}}, "a.example.com"); c.GetCert() != "unknown" {
		t.Errorf("旧版本节点: %+v", c)
	}
	if c := get(&nagiv1.ReportRequest{Node: &nagiv1.NodeInfo{ReportsCerts: true, ProxyDisabled: true}}, "a.example.com"); c.GetCert() != "unknown" {
		t.Errorf("没有 Caddy 的节点: %+v", c)
	}
	if c := get(nil, "a.example.com"); c.GetCert() != "unknown" {
		t.Errorf("还没上报: %+v", c)
	}
}
