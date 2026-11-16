package proxy

import (
	"fmt"
	"testing"
)

// M6：节点 Caddy 的指纹表有上限，不断发起握手不能让它无限变大；新连接的指纹照常记下。
func TestJA4ConnsCapped(t *testing.T) {
	ja4Conns.Lock()
	ja4Conns.m = map[string]ja4Entry{}
	ja4Conns.Unlock()
	for i := range ja4Max * 3 {
		rememberJA4(fmt.Sprintf("198.51.100.%d:%d", i%250, 1024+i/250), "fp")
	}
	ja4Conns.Lock()
	n := len(ja4Conns.m)
	ja4Conns.Unlock()
	if n > ja4Max {
		t.Fatalf("表里有 %d 项，上限是 %d", n, ja4Max)
	}
	rememberJA4("203.0.113.1:5555", "browser")
	if lookupJA4("203.0.113.1:5555") != "browser" {
		t.Fatal("新连接的指纹应该记下")
	}
}
