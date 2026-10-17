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

// 经过 Caddy 的请求按规则统计请求数和 5xx 数。
func TestRouteStats(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
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
	for _, path := range []string{"/", "/", "/fail"} {
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
	if s.Requests != 3 || s.Errors != 1 {
		t.Fatalf("统计不对：%+v", s)
	}
}
