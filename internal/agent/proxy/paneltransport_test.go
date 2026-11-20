package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/quic-go/quic-go/http3"
)

// 面板和项目共用 HTTPS 监听：面板在进入认证前拒绝 HTTP/3，TCP 上仍转发可信指纹；项目继续使用 HTTP/3。
func TestPanelTransport(t *testing.T) {
	var panelRequests atomic.Int64
	panel := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panelRequests.Add(1)
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s:%s:%s", r.Method, body, r.Header.Get(ja4HeaderName))
	}))
	defer panel.Close()
	project := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "project")
	}))
	defer project.Close()

	// 复用生成的生产路由，只把监听移到本机随机端口、证书换成本地测试证书，不访问 ACME。
	cfg := build([]Route{
		{Project: "panel", Protocol: "http", Domains: []string{"panel.example.com"},
			Upstreams:   []string{strings.TrimPrefix(panel.URL, "https://")},
			UpstreamTLS: &UpstreamTLS{CA: panel.Certificate().Raw}},
		{Project: "project", Protocol: "http", Domains: []string{"project.example.com"},
			Upstreams: []string{strings.TrimPrefix(project.URL, "http://")}},
	}, t.TempDir(), func(string) bool { return true })
	apps := cfg["apps"].(obj)
	servers := apps["http"].(obj)["servers"].(obj)
	delete(servers, "http")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp"))
	https := servers["https"].(obj)
	https["listen"] = []string{addr}
	https["automatic_https"] = obj{"disable": true}
	key, err := x509.MarshalPKCS8PrivateKey(panel.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	apps["tls"] = obj{"certificates": obj{"load_pem": []obj{{
		"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: panel.Certificate().Raw})),
		"key":         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
	}}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(data, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()

	roots := x509.NewCertPool()
	roots.AddCert(panel.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots}
	h3 := &http3.Transport{TLSClientConfig: tlsConfig}
	defer h3.Close()
	h2 := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}
	defer h2.CloseIdleConnections()
	h1 := &http.Transport{TLSClientConfig: tlsConfig}
	defer h1.CloseIdleConnections()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, "https://"+addr+"/login", strings.NewReader("payload"))
		req.Host = "panel.example.com"
		req.Header.Set(ja4HeaderName, "forged")
		res, err := (&http.Client{Transport: h3, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.ProtoMajor != 3 || res.StatusCode != http.StatusMisdirectedRequest || res.Header.Get("Alt-Svc") != "clear" {
			t.Errorf("面板 HTTP/3 应拒绝并清除替代服务：%s %d %v", res.Proto, res.StatusCode, res.Header)
		}
		if n := panelRequests.Load(); n != 0 {
			t.Fatalf("HTTP/3 触及面板认证或执行了请求：%d", n)
		}
	}
	for _, c := range []struct {
		name  string
		tr    http.RoundTripper
		major int
	}{{"HTTP/1.1", h1, 1}, {"HTTP/2", h2, 2}} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequest(method, "https://"+addr+"/login", strings.NewReader("payload"))
			req.Host = "panel.example.com"
			req.Header.Set(ja4HeaderName, "forged")
			res, err := (&http.Client{Transport: c.tr, Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.ProtoMajor != c.major || res.StatusCode != http.StatusOK || res.Header.Get("Alt-Svc") != "clear" {
				t.Errorf("%s 应继续服务且不宣传 HTTP/3：%s %d %v", c.name, res.Proto, res.StatusCode, res.Header)
			}
			if prefix := method + ":payload:t"; !strings.HasPrefix(string(body), prefix) || strings.Contains(string(body), "forged") {
				t.Errorf("%s 转发的方法、正文或可信指纹不对：%s", c.name, body)
			}
		}
	}
	if n := panelRequests.Load(); n != 4 {
		t.Fatalf("TCP 请求应各执行一次：%d", n)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	req.Host = "project.example.com"
	res, err := (&http.Client{Transport: h3, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.ProtoMajor != 3 || res.StatusCode != http.StatusOK || string(body) != "project" || res.Header.Get("Alt-Svc") == "clear" {
		t.Fatalf("项目 HTTP/3 不应受影响：%s %d %v %s %v", res.Proto, res.StatusCode, res.Header, body, err)
	}
	// Caddy 在 TCP 响应上宣传 HTTP/3，HTTP/3 响应本身不带 Alt-Svc。
	res, err = (&http.Client{Transport: h2, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 2 || res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Alt-Svc"), "h3=") {
		t.Fatalf("项目仍应宣传 HTTP/3：%s %d %v", res.Proto, res.StatusCode, res.Header)
	}
}
