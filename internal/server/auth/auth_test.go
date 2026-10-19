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
	attempt := func(failed bool) time.Duration {
		done, wait := l.begin("1.2.3.4")
		if done != nil {
			done(failed)
		}
		return wait
	}
	for i := 0; i < 5; i++ {
		if w := attempt(true); w > 0 {
			t.Fatalf("第 %d 次就要等待", i+1)
		}
	}
	if attempt(true) <= 0 {
		t.Fatal("失败 5 次以后应该等待")
	}
	// 同一来源同时只处理一个请求。
	l2 := newLimiter()
	done, _ := l2.begin("5.6.7.8")
	if d, w := l2.begin("5.6.7.8"); d != nil || w != 0 {
		t.Fatal("同一来源的并发请求应该被拒绝")
	}
	done(false)
	if d, _ := l2.begin("5.6.7.8"); d == nil {
		t.Fatal("上一个请求结束后应该可以继续")
	}
	if limitKey("2001:db8::1") != limitKey("2001:db8::ffff") {
		t.Fatal("同一个 /64 的 IPv6 地址应该合并计数")
	}
}

func TestSessionToken(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; "+CookieName+"=tok")
	if got := SessionToken(h); got != "tok" {
		t.Fatalf("got %q", got)
	}
}
