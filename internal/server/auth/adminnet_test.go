package auth

import (
	"context"
	"testing"
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
