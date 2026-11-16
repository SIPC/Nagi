package auth

import (
	"context"
	"fmt"
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

// L10：偷到令牌的人轮流换 21 个以上网段，之前每个都发一条告警，最多只记 20 个网段，本人的网段被挤出去，回来又告警。
// 现在每个令牌每小时只发一条告警；最早用过的网段一直留着。
func TestTokenNetworkAlertFlood(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	admin, _ := h.q.GetUserByUsername(ctx, "admin")
	secret := NewSecret()
	if _, err := h.q.InsertAccessToken(ctx, store.AccessToken{UserID: admin.ID, Name: "laptop", Source: TokenSourcePersonal,
		TokenHash: HashToken(secret), AllowWrite: true, AllowExec: true, CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	// 本人在两个网段用过。
	for _, ip := range []string{"203.0.113.5", "192.0.2.5"} {
		if _, _, err := h.s.TokenUser(ctx, secret, ip); err != nil {
			t.Fatal(err)
		}
	}
	before := len(h.alerts)
	for i := range 40 {
		_, _, _ = h.s.TokenUser(ctx, secret, fmt.Sprintf("198.18.%d.1", i))
	}
	if got := len(h.alerts) - before; got != 0 {
		t.Fatalf("一小时内只发一条告警（本人换网段时已经发过），实际又发了 %d 条", got)
	}
	// 本人回到原来的网段：不告警（没被挤掉）。
	h.alerts = nil
	tokenAlertsReset(h.s)
	for _, ip := range []string{"203.0.113.9", "192.0.2.9"} {
		_, _, _ = h.s.TokenUser(ctx, secret, ip)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("本人一开始用的网段不应被挤掉：%v", h.alerts)
	}
	// 过了一小时，下一条告警带上合并掉的个数。
	_, _, _ = h.s.TokenUser(ctx, secret, "198.19.0.1")
	if len(h.alerts) != 1 {
		t.Fatalf("过了一小时应该再发一条：%v", h.alerts)
	}
}

// tokenAlertsReset 把每个令牌上一次告警的时间往前拨一小时。
func tokenAlertsReset(s *Service) {
	s.tokenAlerts.mu.Lock()
	defer s.tokenAlerts.mu.Unlock()
	for id, t := range s.tokenAlerts.last {
		s.tokenAlerts.last[id] = t.Add(-tokenAlertWindow)
	}
}
