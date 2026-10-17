package proxy

import (
	"encoding/json"
	"testing"

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
