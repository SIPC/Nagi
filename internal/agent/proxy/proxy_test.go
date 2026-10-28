package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	}
	data, err := json.Marshal(build(routes, t.TempDir(), func(d string) bool { return d == "example.com" }))
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Validate(mustConfig(t, data)); err != nil {
		t.Fatalf("invalid caddy config: %v\n%s", err, data)
	}
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
