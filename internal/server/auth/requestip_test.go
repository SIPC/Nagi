package auth

import (
	"net/http"
	"testing"
)

// 只有带着正确 X-Nagi-Proxy、来自本机的请求才采用 X-Forwarded-For。
func TestRequestIPTrustsOnlyOwnProxy(t *testing.T) {
	SetProxySecret("s3cret")
	defer SetProxySecret("")
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.9")
	if got := RequestIP("127.0.0.1:5000", h); got != "127.0.0.1" {
		t.Fatalf("没有 X-Nagi-Proxy 时不应采用 X-Forwarded-For，得到 %s", got)
	}
	h.Set("X-Nagi-Proxy", "wrong")
	if got := RequestIP("127.0.0.1:5000", h); got != "127.0.0.1" {
		t.Fatalf("X-Nagi-Proxy 不对时不应采用 X-Forwarded-For，得到 %s", got)
	}
	h.Set("X-Nagi-Proxy", "s3cret")
	if got := RequestIP("127.0.0.1:5000", h); got != "203.0.113.9" {
		t.Fatalf("自带节点的 Caddy 转发的请求应采用 X-Forwarded-For，得到 %s", got)
	}
	if got := RequestIP("198.51.100.7:5000", h); got != "198.51.100.7" {
		t.Fatalf("不是来自本机的请求不应采用 X-Forwarded-For，得到 %s", got)
	}
}
