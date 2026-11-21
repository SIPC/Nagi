package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/spec"

	"github.com/caddyserver/caddy/v2"
)

// 生成的配置必须能被 Caddy 校验通过（不真正监听端口）。
func TestBuildValidates(t *testing.T) {
	routes := []Route{
		{Project: "p1", Service: "web", Protocol: "http", Domains: []string{"example.com", "pending.example.com"}, Upstreams: []string{"10.200.0.2:80"}},
		{Project: "p1", Service: "web", Protocol: "http", Domains: []string{"plain.example.com"}, HTTPOnly: true, Upstreams: []string{"10.200.0.2:80"}},
		{Project: "p1", Service: "api", Protocol: "http", Published: 18080},
		{Project: "p2", Service: "db", Protocol: "tcp", Published: 15432, Upstreams: []string{"10.200.0.66:5432"}},
		{Project: "p2", Service: "dns", Protocol: "udp", Published: 15353, Upstreams: []string{"10.200.0.67:53"}},
		// 用 HTTPS 连上游、设置请求头（反代面板时）。
		{Project: "p3", Service: "panel", Protocol: "http", Domains: []string{"panel.example.com"}, Upstreams: []string{"127.0.0.1:28443"},
			UpstreamTLS: &UpstreamTLS{CA: testCA(t), ServerName: "panel"}, RequestHeaders: map[string]string{"X-Forwarded-Host": "panel.example.com"}},
	}
	data, err := json.Marshal(build(routes, t.TempDir(), func(d string) bool { return d == "example.com" }))
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Validate(mustConfig(t, data)); err != nil {
		t.Fatalf("invalid caddy config: %v\n%s", err, data)
	}
}

// testCA 生成一张自签的 CA 证书（DER）。
func testCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func mustConfig(t *testing.T, data []byte) *caddy.Config {
	t.Helper()
	var cfg caddy.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

// 经过 Caddy 的请求按规则统计请求数、各类状态码、响应字节和耗时样本。
func TestRouteStats(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			io.WriteString(w, "hello")
		}
	}))
	defer up.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer p.Stop()
	err = p.Apply([]Route{{Project: "p1", Name: "web", Protocol: "http", Published: uint32(port),
		Upstreams: []string{strings.TrimPrefix(up.URL, "http://")}}})
	if err != nil {
		t.Fatal(err)
	}
	TakeStats()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, path := range []string{"/", "/", "/fail", "/missing"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	var s RouteStats
	for range 50 {
		if s = TakeStats()["p1/web"]; s.Requests > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Requests != 4 || s.Errors != 1 || s.Classes[2] != 2 || s.Classes[4] != 1 || s.Classes[5] != 1 {
		t.Fatalf("统计不对：%+v", s)
	}
	if s.Statuses[200] != 2 || s.Statuses[404] != 1 || s.Statuses[500] != 1 || len(s.Statuses) != 3 || s.Timed != 4 || s.Maximum <= 0 {
		t.Fatalf("具体响应码/完整耗时证据不对：%+v", s)
	}
	if s.BytesOut != 10 || len(s.Durations) != 4 {
		t.Fatalf("响应字节或耗时样本不对：BytesOut=%d 样本=%d", s.BytesOut, len(s.Durations))
	}
}

func TestPercentile(t *testing.T) {
	s := RouteStats{}
	if s.Percentile(0.95) != 0 {
		t.Fatal("没有样本时应为 0")
	}
	for i := 100; i >= 1; i-- {
		s.Durations = append(s.Durations, float64(i))
	}
	for q, want := range map[float64]float64{0.5: 50, 0.95: 95, 0.99: 99, 1: 100} {
		if got := s.Percentile(q); got != want {
			t.Errorf("第 %v 分位是 %v，应为 %v", q, got, want)
		}
	}
}

// 80 和 443 只在有域名时才监听，只开 HTTP 的域名不用 443。
func TestDomainPorts(t *testing.T) {
	http80 := spec.PortKey{Proto: spec.ProtoTCP, Port: 80}
	https443 := spec.PortKey{Proto: spec.ProtoTCP, Port: 443}
	cases := []struct {
		name   string
		routes []Route
		want   []spec.PortKey
	}{
		{"没有域名", []Route{{Protocol: "http", Published: 8080}, {Protocol: "tcp", Published: 5432}}, nil},
		{"只开 HTTP", []Route{{Protocol: "http", Domains: []string{"a.example.com"}, HTTPOnly: true}}, []spec.PortKey{http80}},
		{"有证书的域名", []Route{{Protocol: "http", Domains: []string{"a.example.com"}}}, []spec.PortKey{http80, https443}},
		{"不重复", []Route{{Protocol: "http", Domains: []string{"a.example.com"}}, {Protocol: "http", Domains: []string{"b.example.com"}}}, []spec.PortKey{http80, https443}},
	}
	for _, c := range cases {
		if got := domainPorts(c.routes); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// 直连下载：443 上多一条只匹配节点 IP 的规则，IP 的证书向 Let's Encrypt 申请（shortlived），没有 SNI 时按这个 IP 找证书。
// 没有别的规则时也要有 443。生成的配置要能被 Caddy 校验通过。
func TestBuildDirect(t *testing.T) {
	for _, routes := range [][]Route{
		nil,
		{{Project: "p1", Service: "web", Protocol: "http", Domains: []string{"example.com"}, Upstreams: []string{"10.200.0.2:80"}}},
	} {
		cfg := buildWith(routes, t.TempDir(), func(string) bool { return false }, "203.0.113.5")
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := caddy.Validate(mustConfig(t, data)); err != nil {
			t.Fatalf("invalid caddy config: %v\n%s", err, data)
		}
		s := string(data)
		for _, want := range []string{`"handler":"nagi_direct"`, `"host":["203.0.113.5"]`, `"profile":"shortlived"`, `"subjects":["203.0.113.5"]`, `"default_sni":"203.0.113.5"`, `":443"`} {
			if !strings.Contains(s, want) {
				t.Errorf("配置里没有 %s：%s", want, s)
			}
		}
	}
	// 不开直连时没有这些。
	data, _ := json.Marshal(build(nil, t.TempDir(), func(string) bool { return false }))
	if strings.Contains(string(data), "nagi_direct") || strings.Contains(string(data), "shortlived") {
		t.Errorf("没开直连时不该有直连的规则：%s", data)
	}
}

// 加密 HTTP 下载：在 80 上和域名访问共用一个服务，只匹配节点 IP 和下载路径；别的端口单独开一个服务，其他请求 404。
// 生成的配置要能被 Caddy 校验通过。
func TestBuildDirectHTTP(t *testing.T) {
	domain := []Route{{Project: "p1", Service: "web", Protocol: "http", Domains: []string{"example.com"}, Upstreams: []string{"10.200.0.2:80"}}}
	for _, c := range []struct {
		name   string
		routes []Route
		hd     httpDirect
		want   []string
	}{
		{"80，没有域名", nil, httpDirect{"203.0.113.5", 80}, []string{`"listen":[":80"]`, `"path":["/.nagi/dl/*"]`}},
		{"80，有域名", domain, httpDirect{"203.0.113.5", 80}, []string{`"listen":[":80"]`, `"path":["/.nagi/dl/*"]`, `"example.com"`}},
		{"别的端口", domain, httpDirect{"203.0.113.5", 8080}, []string{`"nagi_dl"`, `"listen":[":8080"]`, `"path":["/.nagi/dl/*"]`}},
	} {
		cfg := buildFull(c.routes, t.TempDir(), func(string) bool { return false }, "", c.hd)
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := caddy.Validate(mustConfig(t, data)); err != nil {
			t.Fatalf("%s：invalid caddy config: %v\n%s", c.name, err, data)
		}
		s := string(data)
		for _, want := range append(c.want, `"host":["203.0.113.5"]`, `"handler":"nagi_direct"`) {
			if !strings.Contains(s, want) {
				t.Errorf("%s：配置里没有 %s：%s", c.name, want, s)
			}
		}
		// 不申请 IP 证书（那是 HTTPS 直连的事）。
		if strings.Contains(s, "shortlived") {
			t.Errorf("%s：不该申请证书：%s", c.name, s)
		}
	}
	// 端口被映射占着时不开。
	if !portTaken([]Route{{Protocol: "tcp", Published: 8080}}, 8080) || !portTaken([]Route{{Protocol: "http", Published: 8080}}, 8080) ||
		portTaken([]Route{{Protocol: "udp", Published: 8080}}, 8080) || portTaken(domain, 80) {
		t.Error("portTaken")
	}
}

// 加密 HTTP 下载按需开关时，80 上域名访问的规则照原样不动：开着时只在最前面多一条只匹配节点 IP 和下载路径的规则，
// 其余规则（转发、跳转到 HTTPS、404）和关着时完全一样，HTTPS 的服务也一样。
func TestDirectHTTPKeepsDomainRoutes(t *testing.T) {
	routes := []Route{
		{Project: "p1", Service: "web", Protocol: "http", Domains: []string{"example.com"}, Upstreams: []string{"10.200.0.2:80"}},
		{Project: "p1", Service: "api", Protocol: "http", Domains: []string{"api.example.com"}, HTTPOnly: true, Upstreams: []string{"10.200.0.3:80"}},
	}
	hasCert := func(d string) bool { return d == "example.com" }
	servers := func(hd httpDirect) obj {
		return buildFull(routes, "/x", hasCert, "", hd)["apps"].(obj)["http"].(obj)["servers"].(obj)
	}
	off, on := servers(httpDirect{}), servers(httpDirect{"203.0.113.5", 80})
	offRoutes, onRoutes := off["http"].(obj)["routes"].([]obj), on["http"].(obj)["routes"].([]obj)
	a, _ := json.Marshal(offRoutes)
	b, _ := json.Marshal(onRoutes[1:])
	if len(onRoutes) != len(offRoutes)+1 || string(a) != string(b) {
		t.Fatalf("80 上的域名规则变了：\n%s\n%s", a, b)
	}
	first, _ := json.Marshal(onRoutes[0])
	if !strings.Contains(string(first), `"path":["/.nagi/dl/*"]`) || !strings.Contains(string(first), `"host":["203.0.113.5"]`) {
		t.Errorf("第一条应只匹配节点 IP 和下载路径：%s", first)
	}
	h1, _ := json.Marshal(off["https"])
	h2, _ := json.Marshal(on["https"])
	if string(h1) != string(h2) {
		t.Error("HTTPS 的服务不该变")
	}
}
