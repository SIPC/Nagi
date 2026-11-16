package proxy

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// 反代面板时把客户端的 TLS 指纹转给面板（见 docs/panel.md#会话）：TLS 在 Caddy 这里终结，面板自己看不到浏览器的
// ClientHello。握手匹配器 nagi_ja4 记下每个连接的指纹（总是匹配），处理器 nagi_ja4 把它写进请求头 X-Nagi-Ja4，
// 客户端自己带的同名请求头先删掉。面板只在请求带着正确的 X-Nagi-Proxy 时采用它。

func init() {
	caddy.RegisterModule(ja4Matcher{})
	caddy.RegisterModule(ja4Header{})
}

// ja4Header 是转给面板的请求头。
const ja4HeaderName = "X-Nagi-Ja4"

// ja4Conns 按客户端地址记着连接的指纹。连接上的每个请求都要用，不在读取时删除，按时间清理。
var ja4Conns = struct {
	sync.Mutex
	m    map[string]ja4Entry
	last time.Time
}{m: map[string]ja4Entry{}}

type ja4Entry struct {
	fp string
	at time.Time
}

// ja4Keep 是指纹保留多久：超过它的连接（长连接）的请求不再带指纹，面板这时不做这项检查。
// ja4Max 是最多记多少个连接：没登录的人能不断发起握手，表不能无限变大。满了先删一分钟以前的，还是满的就清空，
// 这些连接之后的请求不带指纹，面板不做这项检查。
const (
	ja4Keep = time.Hour
	ja4Max  = 50000
)

func rememberJA4(addr, fp string) {
	ja4Conns.Lock()
	defer ja4Conns.Unlock()
	now := time.Now()
	if len(ja4Conns.m) >= ja4Max {
		pruneJA4(now, time.Minute)
		if len(ja4Conns.m) >= ja4Max {
			ja4Conns.m = map[string]ja4Entry{}
		}
	}
	ja4Conns.m[addr] = ja4Entry{fp, now}
	if now.Sub(ja4Conns.last) > time.Minute {
		ja4Conns.last = now
		pruneJA4(now, ja4Keep)
	}
}

// pruneJA4 删掉 keep 以前记的。调用方持有锁。
func pruneJA4(now time.Time, keep time.Duration) {
	for k, e := range ja4Conns.m {
		if now.Sub(e.at) > keep {
			delete(ja4Conns.m, k)
		}
	}
}

func lookupJA4(addr string) string {
	ja4Conns.Lock()
	defer ja4Conns.Unlock()
	return ja4Conns.m[addr].fp
}

type ja4Matcher struct{}

func (ja4Matcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "tls.handshake_match.nagi_ja4", New: func() caddy.Module { return new(ja4Matcher) }}
}

// Match 记下指纹，总是匹配。
func (ja4Matcher) Match(h *tls.ClientHelloInfo) bool {
	if h.Conn != nil {
		rememberJA4(h.Conn.RemoteAddr().String(), tlsutil.JA4(h))
	}
	return true
}

type ja4Header struct{}

func (ja4Header) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.nagi_ja4", New: func() caddy.Module { return new(ja4Header) }}
}

func (ja4Header) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	r.Header.Del(ja4HeaderName)
	if r.TLS != nil {
		if fp := lookupJA4(r.RemoteAddr); fp != "" {
			r.Header.Set(ja4HeaderName, fp)
		}
	}
	return next.ServeHTTP(w, r)
}
