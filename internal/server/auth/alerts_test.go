package auth

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/server/store"
)

// 密码正确、验证码连续错误 5 次：告警「密码可能已泄露」，只发一次。
func TestAlertTOTPFailures(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	u, _ := h.q.GetUserByUsername(ctx, "admin")
	if err := h.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString([]byte("JBSWY3DPEHPK3PXP")), ID: u.ID}); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		ctx := WithMeta(ctx, Meta{IP: "198.51.100.9"})
		_, _ = h.s.Login(ctx, "198.51.100."+string(rune('1'+i)), "admin", "correct horse", "000000")
	}
	n := 0
	for _, a := range h.alerts {
		if strings.Contains(a, "密码可能已泄露") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("告警 %d 次：%v", n, h.alerts)
	}
}

// 管理员从新的网段登录时告警；第一次登录、同一网段、操作员不告警；关掉开关以后不告警。
func TestAlertAdminNewNetwork(t *testing.T) {
	h := newSessionHarness(t)
	h.login("admin", home)                                                 // 第一次登录：记下网段
	h.login("admin", RequestInfo{IP: "203.0.113.77", UserAgent: chromeUA}) // 同一 /24
	h.login("op", home)                                                    // 操作员
	h.login("op", office)
	if len(h.alerts) != 0 {
		t.Fatalf("不应告警：%v", h.alerts)
	}
	h.login("admin", office)
	if !h.alerted("从新的网段登录") {
		t.Fatalf("没有告警：%v", h.alerts)
	}
	h.s.NoticeEnabled = func(_ context.Context, n string) bool { return n != NoticeNewNetwork }
	h.alerts = nil
	h.login("admin", RequestInfo{IP: "192.0.2.5", UserAgent: chromeUA})
	if len(h.alerts) != 0 {
		t.Fatalf("关掉以后不应告警：%v", h.alerts)
	}
}

// 按 IP 的登录限速开始等待：整点汇总一条，写明 IP 数和被尝试的用户名。
func TestAlertRateLimitSummary(t *testing.T) {
	h := newSessionHarness(t)
	ctx := WithMeta(context.Background(), Meta{IP: "192.0.2.66"})
	for range ipFreeFailures + 3 {
		// 前 10 次是密码错误，之后开始等待（被限速）。
		_, _ = h.s.Login(ctx, "192.0.2.66", "root", "guess", "")
	}
	h.s.failures.mu.Lock()
	h.s.failures.hour--
	h.s.failures.mu.Unlock()
	h.s.FlushLoginFailures(context.Background())
	if !h.alerted("被限速") || !h.alerted("root") {
		t.Fatalf("没有汇总告警：%v", h.alerts)
	}
}
