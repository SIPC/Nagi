package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse", h) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("wrong password accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	attempt := func(o outcome) time.Duration {
		done, wait := l.begin("1.2.3.4")
		if done != nil {
			done(o)
		}
		return wait
	}
	for i := 0; i < 5; i++ {
		if w := attempt(outcomeFailure); w > 0 {
			t.Fatalf("第 %d 次就要等待", i+1)
		}
	}
	if attempt(outcomeFailure) <= 0 {
		t.Fatal("失败 5 次以后应该等待")
	}
	// 同一来源同时只处理一个请求。
	l2 := newLimiter()
	done, _ := l2.begin("5.6.7.8")
	if d, w := l2.begin("5.6.7.8"); d != nil || w != 0 {
		t.Fatal("同一来源的并发请求应该被拒绝")
	}
	done(outcomeNeutral)
	if d, _ := l2.begin("5.6.7.8"); d == nil {
		t.Fatal("上一个请求结束后应该可以继续")
	}
	if LimitKey("2001:db8::1") != LimitKey("2001:db8::ffff") {
		t.Fatal("同一个 /64 的 IPv6 地址应该合并计数")
	}
}

// 同时校验的密码数按 GOMAXPROCS 算，至少 2 个，最多 4 个（每个要 64 MiB）。
func TestVerifySlots(t *testing.T) {
	for procs, want := range map[int]int{1: 2, 2: 2, 3: 3, 4: 4, 64: 4} {
		if got := verifySlots(procs); got != want {
			t.Errorf("GOMAXPROCS=%d：%d，应该是 %d", procs, got, want)
		}
	}
	if n := cap(verifySem); n < 2 || n > 4 {
		t.Fatalf("verifySem 容量 %d", n)
	}
}

func TestSessionToken(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; "+CookieName+"=tok")
	if got := SessionToken(h); got != "tok" {
		t.Fatalf("got %q", got)
	}
}

// 用自己的账号成功一次、或者发一次「缺少验证码」的请求，都清不掉同一 IP 上猜错的次数；
// 按账号的计数成功后清零；按 IP 的失败过了 failureMemory 才淡出。
func TestLimiterNoResetAbuse(t *testing.T) {
	l := newLimiter()
	try := func(o outcome, keys ...string) time.Duration {
		done, wait := l.begin(keys...)
		if done != nil {
			done(o)
		}
		return wait
	}
	ip := "9.9.9.9"
	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			try(outcomeFailure, ip)
		}
		try(outcomeSuccess, ip) // 自己的账号登录成功
		try(outcomeNeutral, ip) // 密码对了、没带验证码
	}
	if try(outcomeFailure, ip) <= 0 && try(outcomeFailure, ip) <= 0 {
		t.Fatal("成功和不计的请求不应该清掉按 IP 的失败次数")
	}
	// 按账号的计数：成功后清零。
	user := userLimitKey(7)
	for i := 0; i < 4; i++ {
		try(outcomeFailure, user)
	}
	try(outcomeSuccess, user)
	if try(outcomeFailure, user) > 0 {
		t.Fatal("账号成功验证后应该清零")
	}
	// 两个来源一起计：任一被锁定就要等。
	locked := userLimitKey(8)
	for i := 0; i < 6; i++ {
		try(outcomeFailure, "8.8.8.8", locked)
	}
	if try(outcomeFailure, "7.7.7.7", locked) <= 0 {
		t.Fatal("账号被锁定时换 IP 也要等")
	}
	// 淡出：最后一次失败已经过了 failureMemory，而且不在等待中。
	a := l.state[ip]
	a.lastFail, a.until = time.Now().Add(-2*failureMemory), time.Time{}
	if try(outcomeFailure, ip) > 0 {
		t.Fatal("过了 failureMemory 以后应该从头算")
	}
}
