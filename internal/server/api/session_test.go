package api

import (
	"net/http/httptest"
	"testing"
)

// 修改类请求的来源检查（见 docs/panel.md#会话）：兄弟子域名上的网站发来的请求被拒绝。
func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		method, site, origin string
		want                 bool
	}{
		{"POST", "same-origin", "", true},
		{"POST", "same-site", "https://blog.example.com", false},
		{"POST", "cross-site", "https://evil.test", false},
		{"POST", "", "https://panel.example.com", true},
		{"POST", "", "https://blog.example.com", false},
		{"POST", "", "", true}, // 命令行工具：不带浏览器的 cookie
		{"GET", "cross-site", "https://evil.test", true},
	} {
		r := httptest.NewRequest(c.method, "https://panel.example.com/nagi.v1.ProjectService/Deploy", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := sameOrigin(r); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
	for _, p := range []string{"/hooks/abc", "/mcp", "/oauth/token", "/install.sh"} {
		if !originExempt(p) {
			t.Errorf("%s 应不检查来源", p)
		}
	}
}
