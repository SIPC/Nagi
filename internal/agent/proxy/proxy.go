// Package proxy 在节点进程内运行 Caddy，作为所有对外流量的统一出口：
// 域名走 80/443 并自动申请证书，没绑域名的 http 和 tcp/udp 走各自的端口（tcp/udp 用 layer4 模块）。
package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/caddyserver/caddy/v2"
	// 只引入生成的配置用到的模块（见 build），不用 modules/standard：后者还会带进让 Caddy 充当证书机构的
	// ACME 服务端、模板（markdown、语法高亮）、tracing、brotli/zstd 等用不到的模块。
	// 自动 HTTPS 用的是 caddytls 里的 ACME 客户端（向 Let's Encrypt 申请证书），不受影响。
	// 配置里新用到其他模块时在这里加，TestBuildValidates 会把生成的配置交给 Caddy 校验。
	_ "github.com/caddyserver/caddy/v2/modules/caddyevents"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/logging"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"
	_ "github.com/caddyserver/caddy/v2/modules/logging"

	// layer4：tcp/udp 转发。
	_ "github.com/mholt/caddy-l4/layer4"
	_ "github.com/mholt/caddy-l4/modules/l4proxy"
)

// Route 是一条对外开放规则以及它当前的上游地址。
type Route struct {
	Project string
	// Name 是映射文件名，用于统计请求。
	Name    string
	Service string
	// http、tcp 或 udp。
	Protocol  string
	Published uint32
	Domains   []string
	HTTPOnly  bool
	// 上游地址，例如 10.200.0.2:80。容器没运行时为空。
	Upstreams []string
	// UpstreamTLS 不为空时用 HTTPS 连上游（反代面板时用面板的 CA 校验）。
	UpstreamTLS *UpstreamTLS
	// RequestHeaders 是转发给上游时设置的请求头（覆盖客户端带的同名请求头）。
	RequestHeaders map[string]string
	// ProxyProtocol：tcp/udp 转发给上游时在前面加 PROXY protocol v2 头，带着客户端的地址。
	ProxyProtocol bool
}

// UpstreamTLS 是连上游时用的 TLS 设置。
type UpstreamTLS struct {
	CA         []byte // 信任的 CA 证书（DER）
	ServerName string
}

// Proxy 管理进程内的 Caddy。
type Proxy struct {
	storage string
	log     *slog.Logger

	mu    sync.Mutex
	last  []byte
	ports []spec.PortKey // 加载成功的配置里监听的 80 和 443
	// public 是加载成功的配置对外开放的所有端口：80、443（包括 HTTP/3 的 UDP 443）和 TCP/UDP 转发的端口。
	public []spec.PortKey
	// direct 是直连下载用的公网 IP（见 direct.go），directLoaded 是加载成功的配置里的。
	direct, directLoaded string
	// httpDirect 是加密 HTTP 下载的 IP 和端口（见 direct.go），httpLoaded 是加载成功的配置里的；
	// httpErr 是这次没有开的原因（端口被映射占用）。
	httpDirect, httpLoaded httpDirect
	httpErr                string
}

// New 创建代理。storage 是证书等数据的存放目录。
func New(storage string, log *slog.Logger) *Proxy {
	return &Proxy{storage: storage, log: log}
}

// Apply 按给定的规则重新加载 Caddy。配置没变时什么都不做。
func (p *Proxy) Apply(routes []Route) error {
	p.mu.Lock()
	direct, hd := p.direct, p.httpDirect
	p.mu.Unlock()
	// 加密 HTTP 下载的端口被映射占着时不开（同一个端口两个监听，Caddy 的配置整个加载不了）。
	httpErr := ""
	if hd.Address != "" && hd.Port != 80 && portTaken(routes, hd.Port) {
		hd, httpErr = httpDirect{}, i18n.Errorf("端口 %d 已被映射占用", hd.Port).Error()
	}
	cfg, err := json.Marshal(buildFull(routes, p.storage, p.HasCert, direct, hd))
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.httpErr = httpErr
	if bytes.Equal(cfg, p.last) {
		return nil
	}
	if err := caddy.Load(cfg, true); err != nil {
		return fmt.Errorf("加载 Caddy 配置: %w", err)
	}
	p.last = cfg
	p.ports = domainPorts(routes)
	p.public = publicPorts(routes)
	p.directLoaded = direct
	if direct != "" {
		// 直连下载在 443 上（包括 HTTP/3 的 UDP 443）。
		for _, k := range []spec.PortKey{{Proto: spec.ProtoTCP, Port: 443}, {Proto: spec.ProtoUDP, Port: 443}} {
			if !slices.Contains(p.public, k) {
				p.public = append(p.public, k)
			}
		}
		if k := (spec.PortKey{Proto: spec.ProtoTCP, Port: 443}); !slices.Contains(p.ports, k) {
			p.ports = append(p.ports, k)
		}
	}
	p.httpLoaded = hd
	if hd.Address != "" {
		k := spec.PortKey{Proto: spec.ProtoTCP, Port: uint32(hd.Port)}
		if !slices.Contains(p.public, k) {
			p.public = append(p.public, k)
		}
		// 80 和域名访问共用，算 Caddy 自己占着（绑域名时不当作被别的程序占用）。
		if hd.Port == 80 && !slices.Contains(p.ports, k) {
			p.ports = append(p.ports, k)
		}
	}
	p.log.Info("已更新对外访问规则", "routes", len(routes))
	return nil
}

// Reload 强制重新加载当前配置：旧配置的申请任务随之取消，还没拿到证书的域名立刻重新申请，退避从头算。
// 已有证书的域名不受影响，连接和配置变化时的重载一样平滑。
func (p *Proxy) Reload() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		return nil
	}
	if err := caddy.Load(p.last, true); err != nil {
		return fmt.Errorf("加载 Caddy 配置: %w", err)
	}
	return nil
}

// Stop 停止 Caddy。
func (p *Proxy) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last, p.ports, p.directLoaded, p.httpLoaded = nil, nil, "", httpDirect{}
	return caddy.Stop()
}

// DomainPorts 返回 Caddy 实际监听着的 80 和 443。配置没加载成功时（例如端口被别的程序占着）不算。
func (p *Proxy) DomainPorts() []spec.PortKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ports)
}

// PublicPorts 返回 Caddy 现在实际监听着的对外端口。容器出站限制放行发往节点本机公网地址这些端口的连接：
// 它们本来就对公网开放，容器访问自己或别的项目的域名时会连到节点自己的地址。
func (p *Proxy) PublicPorts() []spec.PortKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.public)
}

func publicPorts(routes []Route) []spec.PortKey {
	out := domainPorts(routes)
	if slices.Contains(out, spec.PortKey{Proto: spec.ProtoTCP, Port: 443}) {
		out = append(out, spec.PortKey{Proto: spec.ProtoUDP, Port: 443})
	}
	for _, r := range routes {
		// 没有上游的 TCP/UDP 规则 Caddy 不监听（见 build）：放行它们的话，之后占用这个端口的别的程序也能被容器连到。
		if (r.Protocol == spec.ProtoTCP || r.Protocol == spec.ProtoUDP) && r.Published > 0 && len(r.Upstreams) > 0 {
			if k := (spec.PortKey{Proto: r.Protocol, Port: r.Published}); !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// domainPorts 返回这些规则要监听的 80 和 443，和 build 里的一致。
func domainPorts(routes []Route) []spec.PortKey {
	var out []spec.PortKey
	add := func(port uint32) {
		if k := (spec.PortKey{Proto: spec.ProtoTCP, Port: port}); !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	for _, r := range routes {
		if r.Protocol == spec.ProtoHTTP && len(r.Domains) > 0 {
			add(80)
			if !r.HTTPOnly {
				add(443)
			}
		}
	}
	return out
}

type obj = map[string]any

// HasCert 判断某个域名是否已经拿到证书（Caddy 把证书存在 certificates/<签发机构>/<域名>/ 下）。
func (p *Proxy) HasCert(domain string) bool {
	matches, _ := filepath.Glob(filepath.Join(p.storage, "certificates", "*", domain, domain+".crt"))
	return len(matches) > 0
}

// build 生成 Caddy 的 JSON 配置。hasCert 判断域名是否已经拿到证书。
//
// 绑了域名的 http：先只开 HTTP；拿到证书以后同时开 HTTPS，并把 HTTP 跳转到 HTTPS。
// 所以关闭 Caddy 自带的自动跳转，由这里按证书状态决定 80 端口上是转发还是跳转。
func build(routes []Route, storage string, hasCert func(string) bool) obj {
	return buildWith(routes, storage, hasCert, "")
}

// buildWith 和 build 一样，direct 不为空时在 443 上为这个 IP 开直连下载（见 direct.go）。
func buildWith(routes []Route, storage string, hasCert func(string) bool, direct string) obj {
	return buildFull(routes, storage, hasCert, direct, httpDirect{})
}

// buildFull 和 buildWith 一样，hd 不为空时再开加密 HTTP 下载（见 direct.go）。
func buildFull(routes []Route, storage string, hasCert func(string) bool, direct string, hd httpDirect) obj {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Project != routes[j].Project {
			return routes[i].Project < routes[j].Project
		}
		return routes[i].Published < routes[j].Published
	})

	httpServers := obj{}
	l4Servers := obj{}
	var httpsRoutes, port80Routes []obj

	for _, r := range routes {
		switch r.Protocol {
		case "http":
			if len(r.Domains) > 0 {
				proxyRoute := func(hosts []string) obj {
					return obj{"match": []obj{{"host": hosts}}, "handle": handlers(r), "terminal": true}
				}
				if r.HTTPOnly {
					port80Routes = append(port80Routes, proxyRoute(r.Domains))
					continue
				}
				// 443 上始终包含这些域名，Caddy 才会去申请证书。
				httpsRoutes = append(httpsRoutes, proxyRoute(r.Domains))
				var ready, pending []string
				for _, d := range r.Domains {
					if hasCert(d) {
						ready = append(ready, d)
					} else {
						pending = append(pending, d)
					}
				}
				if len(ready) > 0 {
					port80Routes = append(port80Routes, obj{
						"match":    []obj{{"host": ready}},
						"handle":   []obj{{"handler": "static_response", "status_code": 308, "headers": obj{"Location": []string{"https://{http.request.host}{http.request.uri}"}}}},
						"terminal": true,
					})
				}
				if len(pending) > 0 {
					port80Routes = append(port80Routes, proxyRoute(pending))
				}
				continue
			}
			httpServers["port_"+strconv.Itoa(int(r.Published))] = obj{
				"listen":          []string{":" + strconv.Itoa(int(r.Published))},
				"routes":          []obj{{"handle": handlers(r)}},
				"automatic_https": obj{"disable": true},
				"logs":            obj{},
			}
		case "tcp", "udp":
			if len(r.Upstreams) == 0 {
				continue
			}
			var dial []string
			for _, u := range r.Upstreams {
				if r.Protocol == "udp" {
					dial = append(dial, "udp/"+u)
				} else {
					dial = append(dial, u)
				}
			}
			h := obj{"handler": "proxy", "upstreams": []obj{{"dial": dial}}}
			if r.ProxyProtocol {
				h["proxy_protocol"] = "v2"
			}
			l4Servers[r.Protocol+"_"+strconv.Itoa(int(r.Published))] = obj{
				"listen": []string{r.Protocol + "/:" + strconv.Itoa(int(r.Published))},
				// 先经过 nagi_stats 统计连接和流量（见 l4stats.go）。
				"routes": []obj{{"handle": []obj{{"handler": "nagi_stats", "route": r.Project + "/" + r.Name}, h}}},
			}
		}
	}

	if direct != "" {
		httpsRoutes = append(httpsRoutes, directRoute(direct))
	}
	if len(httpsRoutes) > 0 {
		// 握手时记下 TLS 指纹，反代面板时转给面板（见 ja4.go）。
		policy := obj{"match": obj{"nagi_ja4": obj{}}}
		if direct != "" {
			// 浏览器连 IP 时不发 SNI：按这个 IP 找证书。节点在 NAT 后面（端口转发）时，连接的本地地址是内网地址，
			// Caddy 默认按它找不到证书。
			policy["default_sni"] = direct
		}
		httpServers["https"] = obj{
			"listen":                  []string{":443"},
			"routes":                  httpsRoutes,
			"automatic_https":         obj{"disable_redirects": true},
			"logs":                    obj{},
			"tls_connection_policies": []obj{policy},
		}
	}
	// 加密 HTTP 下载：在 80 上时和域名访问共用一个服务，加一条只匹配节点 IP 和下载路径的规则；
	// 别的端口单独开一个服务，其他请求一律 404。
	if hd.Address != "" {
		if hd.Port == 80 {
			port80Routes = append([]obj{httpDirectRoute(hd.Address)}, port80Routes...)
		} else {
			httpServers["nagi_dl"] = obj{
				"listen":          []string{":" + strconv.Itoa(hd.Port)},
				"routes":          []obj{httpDirectRoute(hd.Address), {"handle": []obj{{"handler": "static_response", "status_code": 404}}}},
				"automatic_https": obj{"disable": true},
				"logs":            obj{},
			}
		}
	}
	// 80 端口：只开 HTTP 的域名、还没拿到证书的域名直接转发；已有证书的域名跳转到 HTTPS；
	// 证书申请的 HTTP 验证由 Caddy 自动处理。没匹配到任何域名的请求返回 404。
	if len(port80Routes) > 0 {
		httpServers["http"] = obj{"listen": []string{":80"}, "routes": append(port80Routes, notFound()), "logs": obj{}}
	}

	// 证书申请成功或失败时通知节点（见 observe.go）。
	apps := obj{"nagi_cert_events": obj{}}
	if len(httpServers) > 0 {
		apps["http"] = obj{"servers": httpServers}
	}
	if len(l4Servers) > 0 {
		apps["layer4"] = obj{"servers": l4Servers}
	}
	if direct != "" {
		apps["tls"] = directTLS(direct)
	}
	return obj{
		"admin": obj{"disabled": true, "config": obj{"persist": false}},
		// 访问日志不输出，只交给 nagi_metrics 统计请求数、5xx 和耗时。
		"logging": obj{"logs": obj{
			"default": obj{"level": "WARN", "exclude": []string{"http.log.access"}},
			"nagi_access": obj{
				"writer":  obj{"output": "nagi_metrics"},
				"encoder": obj{"format": "json"},
				"include": []string{"http.log.access"},
			},
		}},
		"storage": obj{"module": "file_system", "root": storage},
		"apps":    apps,
	}
}

// handlers 返回一条 http 规则的处理器：先在访问日志里标上规则，再反向代理。
func handlers(r Route) []obj {
	h := []obj{{"handler": "log_append", "key": logRouteKey, "value": r.Project + "/" + r.Name}}
	// 反代面板（UpstreamTLS）时带上客户端的 TLS 指纹。
	if r.UpstreamTLS != nil {
		h = append(h, obj{"handler": "nagi_ja4"})
	}
	return append(h, withHeaders(reverseProxyTLS(r.Upstreams, r.UpstreamTLS), r.RequestHeaders))
}

// withHeaders 让反向代理转发时设置这些请求头。
func withHeaders(h obj, headers map[string]string) obj {
	if len(headers) == 0 || h["handler"] != "reverse_proxy" {
		return h
	}
	set := obj{}
	for k, v := range headers {
		set[k] = []string{v}
	}
	h["headers"] = obj{"request": obj{"set": set}}
	return h
}

func notFound() obj {
	return obj{"handle": []obj{{"handler": "static_response", "status_code": 404, "body": "未找到这个域名对应的服务"}}}
}

func reverseProxyTLS(upstreams []string, t *UpstreamTLS) obj {
	h := reverseProxy(upstreams)
	if t != nil && h["handler"] == "reverse_proxy" {
		h["transport"] = obj{"protocol": "http", "tls": obj{
			"ca":          obj{"provider": "inline", "trusted_ca_certs": []string{base64.StdEncoding.EncodeToString(t.CA)}},
			"server_name": t.ServerName,
		}}
	}
	return h
}

// reverseProxy 返回反向代理处理器；容器没运行时返回 502 提示。
func reverseProxy(upstreams []string) obj {
	if len(upstreams) == 0 {
		return obj{
			"handler":     "static_response",
			"status_code": 502,
			"body":        "服务未运行",
		}
	}
	var ups []obj
	for _, u := range upstreams {
		ups = append(ups, obj{"dial": u})
	}
	return obj{"handler": "reverse_proxy", "upstreams": ups}
}
