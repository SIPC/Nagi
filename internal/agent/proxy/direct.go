package proxy

import (
	"net/http"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// 直连下载（见 docs/agent.md#直连下载）：浏览器拿着面板签的链接直接从节点下载文件。Caddy 在 443 上为节点的公网 IP
// 申请 Let's Encrypt 的 IP 证书（只能用 shortlived 配置，6 天有效、自动续期），这个 IP 上的请求交给 Caddy 模块
// nagi_direct，由节点进程处理（核对签名、发文件）。Caddy 模块拿不到节点，所以经包级变量传递，和 nagi_metrics 一样。

func init() {
	caddy.RegisterModule(directModule{})
}

var directHandler atomic.Pointer[http.Handler]

// SetDirectHandler 设置处理直连下载请求的 handler。
func SetDirectHandler(h http.Handler) {
	directHandler.Store(&h)
}

type directModule struct{}

func (directModule) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nagi_direct",
		New: func() caddy.Module { return new(directModule) },
	}
}

func (directModule) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if h := directHandler.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return nil
	}
	http.NotFound(w, r)
	return nil
}

// SetDirect 设置直连下载用的公网 IP（为空表示不开），下次 Apply 时生效。
func (p *Proxy) SetDirect(address string) {
	p.mu.Lock()
	p.direct = address
	p.mu.Unlock()
}

// Direct 返回加载成功的配置里直连下载用的 IP（没有开或者配置没加载成功时为空）。
func (p *Proxy) Direct() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.directLoaded
}

// directRoute 是 443 上直连下载的规则：只匹配节点的 IP。
func directRoute(address string) obj {
	return obj{"match": []obj{{"host": []string{address}}}, "handle": []obj{{"handler": "nagi_direct"}}, "terminal": true}
}

// directTLS 让 Caddy 向 Let's Encrypt 申请这个 IP 的证书：IP 证书只能用 shortlived 配置。
// 不写的话 Caddy 给 IP 用自己的内部 CA 签，浏览器不认。
func directTLS(address string) obj {
	return obj{"automation": obj{"policies": []obj{{
		"subjects": []string{address},
		"issuers":  []obj{DirectIssuer},
	}}}}
}

// httpDirect 是加密 HTTP 下载在 Caddy 里的规则：节点的 IP 和端口，Address 为空表示不开。
type httpDirect struct {
	Address string
	Port    int
}

// SetDirectHTTP 设置加密 HTTP 下载（见 docs/agent.md#加密-http-下载）的 IP 和端口，address 为空表示不开，下次 Apply 时生效。
// 请求和 HTTPS 的直连下载交给同一个 handler，由它按路径区分。
func (p *Proxy) SetDirectHTTP(address string, port int) {
	p.mu.Lock()
	p.httpDirect = httpDirect{Address: address, Port: port}
	if address == "" {
		p.httpDirect = httpDirect{}
	}
	p.mu.Unlock()
}

// DirectHTTP 返回加载成功的配置里加密 HTTP 下载的 IP 和端口（没有开时 IP 为空），
// 以及这次没有开的原因（端口被映射占用时）。
func (p *Proxy) DirectHTTP() (address string, port int, err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.httpLoaded.Address, p.httpLoaded.Port, p.httpErr
}

// httpDirectRoute 是加密 HTTP 下载的规则：只匹配节点的 IP 和下载路径。
func httpDirectRoute(address string) obj {
	return obj{"match": []obj{{"host": []string{address}, "path": []string{"/.nagi/dl/*"}}}, "handle": []obj{{"handler": "nagi_direct"}}, "terminal": true}
}

// portTaken 判断 TCP 端口 port 是不是已经有映射在用（没绑域名的 http、tcp）。
func portTaken(routes []Route, port int) bool {
	for _, r := range routes {
		if int(r.Published) != port || r.Protocol == "udp" || (r.Protocol == "http" && len(r.Domains) > 0) {
			continue
		}
		return true
	}
	return false
}

// DirectIssuer 是直连下载的 IP 证书的签发方。测试时换成 Caddy 的内部 CA（不用联网）。
var DirectIssuer = obj{"module": "acme", "profile": "shortlived"}

var _ caddyhttp.MiddlewareHandler = directModule{}
