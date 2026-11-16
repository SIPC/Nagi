package auth

import (
	"context"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/store"
)

func TestTokenNetworks(t *testing.T) {
	h := newSessionHarness(t)
	ctx := WithMeta(context.Background(), Meta{Client: "claude-code/2.0"})
	admin, _ := h.q.GetUserByUsername(ctx, "admin")
	add := func(name string, exec bool, networks []string) string {
		nets, err := TokenNetworks(networks)
		if err != nil {
			t.Fatal(err)
		}
		secret := NewSecret()
		if _, err := h.q.InsertAccessToken(ctx, store.AccessToken{UserID: admin.ID, Name: name, Source: TokenSourcePersonal,
			TokenHash: HashToken(secret), AllowWrite: true, AllowExec: exec, CreatedAt: time.Now().Unix(), Networks: nets}); err != nil {
			t.Fatal(err)
		}
		return secret
	}
	if _, err := TokenNetworks([]string{"bad"}); err == nil {
		t.Fatal("格式不对应该拒绝")
	}

	// 限制网段：范围内能用，范围外当作无效并告警。
	limited := add("ci", false, []string{"192.0.2.0/24"})
	if _, _, err := h.s.TokenUser(ctx, limited, "192.0.2.10"); err != nil {
		t.Fatalf("范围内应该能用：%v", err)
	}
	if _, _, err := h.s.TokenUser(ctx, limited, "198.51.100.1"); err == nil {
		t.Fatal("范围外不能用")
	}
	if !h.alerted("198.51.100.1") {
		t.Fatalf("范围外使用应该告警：%v", h.alerts)
	}

	// 能执行命令的令牌：第一次使用不告警，换到新网段告警一次，客户端记下来。
	exec := add("laptop", true, nil)
	if _, _, err := h.s.TokenUser(ctx, exec, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	if h.alerted("203.0.113.0/24") {
		t.Fatal("第一次使用不告警")
	}
	if _, tok, _ := h.s.TokenUser(ctx, exec, "203.0.113.9"); tok.LastClient != "claude-code/2.0" {
		t.Fatalf("同一网段不告警，客户端要记下：%q", tok.LastClient)
	}
	if _, _, err := h.s.TokenUser(ctx, exec, "198.51.100.77"); err != nil {
		t.Fatal(err)
	}
	if !h.alerted("198.51.100.77") {
		t.Fatalf("新网段应该告警：%v", h.alerts)
	}
	n := len(h.alerts)
	_, _, _ = h.s.TokenUser(ctx, exec, "198.51.100.78")
	if len(h.alerts) != n {
		t.Fatal("同一个新网段只告警一次")
	}

	// 不能执行命令的令牌换网段不告警。
	read := add("read", false, nil)
	_, _, _ = h.s.TokenUser(ctx, read, "203.0.113.5")
	_, _, _ = h.s.TokenUser(ctx, read, "100.64.0.1")
	if h.alerted("100.64.0.1") {
		t.Fatal("只读令牌换网段不告警")
	}
}
