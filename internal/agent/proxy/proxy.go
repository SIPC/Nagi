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
	"sort"
	"strconv"
	"sync"

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

	mu   sync.Mutex
	last []byte
}

// New 创建代理。storage 是证书等数据的存放目录。
func New(storage string, log *slog.Logger) *Proxy {
	return &Proxy{storage: storage, log: log}
}

// Apply 按给定的规则重新加载 Caddy。配置没变时什么都不做。
func (p *Proxy) Apply(routes []Route) error {
	cfg, err := json.Marshal(build(routes, p.storage, p.hasCert))
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if bytes.Equal(cfg, p.last) {
		return nil
	}
	if err := caddy.Load(cfg, true); err != nil {
		return fmt.Errorf("加载 Caddy 配置: %w", err)
	}
	p.last = cfg
	p.log.Info("已更新对外访问规则", "routes", len(routes))
	return nil
}

// Stop 停止 Caddy。
func (p *Proxy) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = nil
	return caddy.Stop()
}

type obj = map[string]any

// hasCert 判断某个域名是否已经拿到证书（Caddy 把证书存在 certificates/<签发机构>/<域名>/ 下）。
func (p *Proxy) hasCert(domain string) bool {
	matches, _ := filepath.Glob(filepath.Join(p.storage, "certificates", "*", domain, domain+".crt"))
	return len(matches) > 0
}

// build 生成 Caddy 的 JSON 配置。hasCert 判断域名是否已经拿到证书。
//
// 绑了域名的 http：先只开 HTTP；拿到证书以后同时开 HTTPS，并把 HTTP 跳转到 HTTPS。
// 所以关闭 Caddy 自带的自动跳转，由这里按证书状态决定 80 端口上是转发还是跳转。
func build(routes []Route, storage string, hasCert func(string) bool) obj {
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
			l4Servers[r.Protocol+"_"+strconv.Itoa(int(r.Published))] = obj{
				"listen": []string{r.Protocol + "/:" + strconv.Itoa(int(r.Published))},
				"routes": []obj{{"handle": []obj{{
					"handler":   "proxy",
					"upstreams": []obj{{"dial": dial}},
				}}}},
			}
		}
	}

	if len(httpsRoutes) > 0 {
		httpServers["https"] = obj{
			"listen":          []string{":443"},
			"routes":          httpsRoutes,
			"automatic_https": obj{"disable_redirects": true},
			"logs":            obj{},
		}
	}
	// 80 端口：只开 HTTP 的域名、还没拿到证书的域名直接转发；已有证书的域名跳转到 HTTPS；
	// 证书申请的 HTTP 验证由 Caddy 自动处理。没匹配到任何域名的请求返回 404。
	if len(port80Routes) > 0 {
		httpServers["http"] = obj{"listen": []string{":80"}, "routes": append(port80Routes, notFound()), "logs": obj{}}
	}

	// 证书申请成功或失败时通知节点（见 observe.go）。
	apps := obj{"events": obj{"subscriptions": []obj{{
		"events":   []string{"cert_obtained", "cert_failed"},
		"handlers": []obj{{"handler": "nagi"}},
	}}}}
	if len(httpServers) > 0 {
		apps["http"] = obj{"servers": httpServers}
	}
	if len(l4Servers) > 0 {
		apps["layer4"] = obj{"servers": l4Servers}
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
	return []obj{
		{"handler": "log_append", "key": logRouteKey, "value": r.Project + "/" + r.Name},
		withHeaders(reverseProxyTLS(r.Upstreams, r.UpstreamTLS), r.RequestHeaders),
	}
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
