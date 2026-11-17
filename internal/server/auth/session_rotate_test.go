package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// 页面拿到挑战以后、提交签名之前，挑战换了（过期再发一次）：用旧挑战的签名不算签名不符，
// 不要求重新验证、不告警；之后用新挑战签名照常换发。
func TestSessionRotateStaleChallenge(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	key := newBrowserKey()
	token := h.login("admin", home)
	if err := h.s.RegisterSessionKey(ctx, token, key.spki()); err != nil {
		t.Fatal(err)
	}
	h.age(token, RotateInterval)
	first, err := h.check(token, home)
	if err != nil || first.challenge == "" {
		t.Fatalf("应收到挑战：%v", err)
	}
	// 标签页在后台，签名晚了：挑战过期，下一个请求收到再发的挑战。
	h.age(token, challengeGrace+time.Second)
	second, err := h.check(token, home)
	if err != nil || second.challenge == "" || second.challenge == first.challenge {
		t.Fatalf("应再发一次挑战：%q %v", second.challenge, err)
	}
	// 后台的页面这时才交上旧挑战的签名。
	_, err = h.s.RotateSession(ctx, token, first.challenge, key.sign(first.challenge), home)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("旧挑战的签名应只是不换发：%v", err)
	}
	var rv *ErrReverify
	if _, err := h.check(token, home); errors.As(err, &rv) {
		t.Fatalf("旧挑战的签名不应要求重新验证：%v", err)
	}
	if h.alerted("会话密钥") {
		t.Fatalf("旧挑战的签名不应告警：%v", h.alerts)
	}
	// 用新挑战签名：照常换发。
	next, err := h.s.RotateSession(ctx, token, second.challenge, key.sign(second.challenge), home)
	if err != nil || next == "" {
		t.Fatalf("新挑战的签名应换发：%v", err)
	}
	// 挑战对得上、签名不对：仍然是签名不符。
	token2 := h.login("op", home)
	_ = h.s.RegisterSessionKey(ctx, token2, key.spki())
	h.age(token2, RotateInterval)
	res, _ := h.check(token2, home)
	if _, err := h.s.RotateSession(ctx, token2, res.challenge, newBrowserKey().sign(res.challenge), home); err == nil {
		t.Fatal("签名不对也换发了")
	}
	if _, err := h.check(token2, home); !errors.As(err, &rv) {
		t.Fatalf("签名不符应要求重新验证：%v", err)
	}
}
