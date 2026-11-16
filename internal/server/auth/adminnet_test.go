package auth

import (
	"context"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/store"
)

func TestAdminNetworks(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	inside := RequestInfo{IP: "203.0.113.7", UserAgent: chromeUA, Fingerprint: chromeFP}
	outside := RequestInfo{IP: "198.51.100.9", UserAgent: chromeUA, Fingerprint: chromeFP}

	// 不能把当前地址排除在外；格式不对的拒绝。
	if _, err := h.s.SetAdminNetworks(ctx, []string{"10.0.0.0/8"}, inside.IP); err == nil {
		t.Fatal("当前地址不在列表里时不能保存")
	}
	if _, err := h.s.SetAdminNetworks(ctx, []string{"not-an-ip"}, inside.IP); err == nil {
		t.Fatal("格式不对应该拒绝")
	}
	got, err := h.s.SetAdminNetworks(ctx, []string{" 203.0.113.9/24 ", "2001:db8::1", "203.0.113.0/24"}, inside.IP)
	if err != nil || len(got) != 2 || got[0] != "203.0.113.0/24" || got[1] != "2001:db8::1" {
		t.Fatalf("保存的列表不对：%v %v", got, err)
	}

	// 先在允许的地址登录。
	token := h.login("admin", inside)
	if _, err := h.check(token, inside); err != nil {
		t.Fatalf("允许的地址应该能用：%v", err)
	}
	// 同一个会话换到别的地址：不能用，回来以后照常。
	if _, err := h.check(token, outside); err == nil {
		t.Fatal("允许范围以外的地址不能用管理员的会话")
	}
	if _, err := h.check(token, inside); err != nil {
		t.Fatalf("回到允许的地址应该照常：%v", err)
	}
	// 从别的地址用正确的密码登录：拒绝并告警。
	octx := WithMeta(ctx, Meta{IP: outside.IP, UserAgent: outside.UserAgent, Fingerprint: outside.Fingerprint})
	if _, err := h.s.Login(octx, outside.IP, "admin", "correct horse", ""); err == nil {
		t.Fatal("允许范围以外的地址不能登录管理员")
	}
	if !h.alerted(outside.IP) {
		t.Fatalf("应该告警：%v", h.alerts)
	}
	// 其他角色不受影响。
	if _, err := h.s.Login(octx, outside.IP, "op", "correct horse", ""); err != nil {
		t.Fatalf("操作员不受限制：%v", err)
	}
	// 清空就是不限制。
	if _, err := h.s.SetAdminNetworks(ctx, nil, outside.IP); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Login(octx, outside.IP, "admin", "correct horse", ""); err != nil {
		t.Fatalf("清除以后应该能登录：%v", err)
	}
}

// M4：设了管理员登录地址以后，管理员的访问令牌（个人令牌和 OAuth 授权的）从范围以外的地址使用时当作无效并告警；
// 操作员的令牌不受影响。
func TestAdminNetworksTokens(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	if _, err := h.s.SetAdminNetworks(ctx, []string{"203.0.113.0/24"}, "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	add := func(user, source string) string {
		u, _ := h.q.GetUserByUsername(ctx, user)
		secret := NewSecret()
		if _, err := h.q.InsertAccessToken(ctx, store.AccessToken{UserID: u.ID, Name: user + "-" + source, Source: source,
			TokenHash: HashToken(secret), AllowWrite: true, AllowExec: true, AllowNodeExec: true, CreatedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
		return secret
	}
	for _, source := range []string{TokenSourcePersonal, TokenSourceOAuth} {
		tok := add("admin", source)
		if _, _, err := h.s.TokenUser(ctx, tok, "203.0.113.20"); err != nil {
			t.Fatalf("%s：允许的地址应该能用：%v", source, err)
		}
		if _, _, err := h.s.TokenUser(ctx, tok, "198.51.100.9"); err != ErrInvalidToken {
			t.Fatalf("%s：允许范围以外的地址应该当作无效，实际 %v", source, err)
		}
	}
	if !h.alerted("198.51.100.9") {
		t.Fatalf("应该告警：%v", h.alerts)
	}
	op := add("op", TokenSourcePersonal)
	if _, _, err := h.s.TokenUser(ctx, op, "198.51.100.9"); err != nil {
		t.Fatalf("操作员的令牌不受管理员登录地址影响：%v", err)
	}
}
