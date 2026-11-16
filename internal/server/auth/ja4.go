package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
)

// TLS 指纹（见 docs/panel.md#会话）：按 JA4 的规则从 ClientHello 算出，用来判断对方是不是真的浏览器。
// 数据在服务端拿到，客户端改不了上报的内容。

// JA4 按 JA4 的规则算 ClientHello 的指纹，见 tlsutil.JA4。
func JA4(h *tls.ClientHelloInfo) string { return tlsutil.JA4(h) }

// toolFingerprints 是脚本工具的已知指纹（JA4 的 a、b 两段）：Go、Python、curl、Node 等。
// 浏览器的指纹都声明了 ALPN、扩展较多，单靠这份列表认不全工具，所以另有 looksLikeTool。
var toolFingerprints = map[string]string{}

// looksLikeTool 判断指纹是不是来自脚本工具而不是浏览器。浏览器（Chrome、Edge、Firefox、Safari）的 ClientHello
// 扩展在 14 个以上，并且声明 ALPN；Go、Python、curl、Node 的 HTTP 客户端扩展少得多，Python 不声明 ALPN。
// 不按 ALPN 是不是 h2 判断：浏览器为 WebSocket（节点终端、容器终端）另开的连接只声明 http/1.1（h1），
// 走 HTTP/3 时声明 h3，都是浏览器。
func looksLikeTool(fp string) bool {
	if fp == "" {
		return false
	}
	parts := strings.SplitN(fp, "_", 3)
	if len(parts) != 3 || len(parts[0]) != 10 {
		return false
	}
	if _, ok := toolFingerprints[parts[0]+"_"+parts[1]]; ok {
		return true
	}
	a := parts[0]
	var exts int
	fmt.Sscanf(a[6:8], "%d", &exts)
	return a[8:] == "00" || exts < 14
}

// sameClient 判断两次握手是不是同一个客户端：只比较同一个浏览器每次连接都不变的部分，即 TLS 版本、SNI、
// 密码套件个数和密码套件的哈希（JA4 a 段的前 6 位，加上 b 段）。这些不比较：
//   - 扩展的个数和哈希（c 段）：会话恢复时多出 pre_shared_key（有时还有 early_data），有的握手带 padding，
//     同一个浏览器两次连接也会不同，手机网络下重连频繁，按完整指纹比较会反复要求重新验证；
//   - ALPN：页面走 h2，WebSocket（终端）另开的连接是 h1，HTTP/3 是 h3。
func sameClient(a, b string) bool { return clientKey(a) == clientKey(b) }

func clientKey(fp string) string {
	parts := strings.SplitN(fp, "_", 3)
	if len(parts) != 3 || len(parts[0]) != 10 {
		return fp
	}
	return parts[0][:6] + "_" + parts[1]
}

// fingerprints 记着每个连接的指纹：握手时按底层连接记下（GetConfigForClient 里只有底层连接），握手结束后
// 不管成败马上取走（TakeFingerprint），要交给 HTTP 服务的再按 TLS 连接放回去（HoldFingerprint），HTTP 服务拿到
// 连接时取走。表里只有正在握手和排队等 HTTP 服务接收的连接：握手失败、中途断开、节点和备用面板的连接都不会留下，
// 没登录的人不能借反复中止握手让它一直变大。
var fingerprints sync.Map // net.Conn → string

// RecordFingerprint 记下底层连接的 ClientHello 指纹，在 GetConfigForClient 里调用。
func RecordFingerprint(h *tls.ClientHelloInfo) {
	if h.Conn != nil {
		fingerprints.Store(h.Conn, JA4(h))
	}
}

// HoldFingerprint 把握手后取出的指纹按 TLS 连接放回去，等 HTTP 服务的 ConnContext 取走。
func HoldFingerprint(c net.Conn, fp string) {
	if fp != "" {
		fingerprints.Store(c, fp)
	}
}

// FingerprintCount 是指纹表里的项数，测试用。
func FingerprintCount() int {
	n := 0
	fingerprints.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TakeFingerprint 取走连接的指纹。
func TakeFingerprint(raw net.Conn) string {
	v, ok := fingerprints.LoadAndDelete(raw)
	if !ok {
		return ""
	}
	return v.(string)
}

type fpKey struct{}

// WithFingerprint 把连接的 TLS 指纹放进 ctx（http.Server.ConnContext 里调用）。
func WithFingerprint(ctx context.Context, fp string) context.Context {
	return context.WithValue(ctx, fpKey{}, fp)
}

// fingerprintHeader 是本机 Caddy 转发时带上的 TLS 指纹（由 Nagi 自己的 Caddy 模块算好，见 agent/proxy）。
const fingerprintHeader = "X-Nagi-Ja4"

// cdnHeaders 说明请求经过了 CDN：TLS 由 CDN 终结，看到的是 CDN 的指纹，不检查。
var cdnHeaders = []string{"Cf-Ray", "Cdn-Loop", "X-Amz-Cf-Id", "Fastly-Client-Ip", "Ali-Cdn-Real-Ip", "X-Tencent-Ua", "Eagleid"}

// RequestFingerprint 返回请求的 TLS 指纹：经本机 Caddy 转发时采用它算好的（凭 X-Nagi-Proxy 核对），否则用面板自己
// 握手时算的。经过 CDN 的、开发时本机代理转发的（来自本机、不是 Caddy）返回空，不做这项检查。
func RequestFingerprint(ctx context.Context, addr string, h http.Header) string {
	for _, k := range cdnHeaders {
		if h.Get(k) != "" {
			return ""
		}
	}
	if fromLocalProxy(addr, h) {
		return h.Get(fingerprintHeader)
	}
	if ip := net.ParseIP(ClientIP(addr)); ip != nil && ip.IsLoopback() {
		return ""
	}
	fp, _ := ctx.Value(fpKey{}).(string)
	return fp
}
