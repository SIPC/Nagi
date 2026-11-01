package agent

import (
	"errors"
	"testing"
	"time"
)

// 手动重试要限流：Let's Encrypt 对失败的验证有次数限制。
func TestCertRetryRateLimit(t *testing.T) {
	f := &certFailures{}
	f.set("a.example.com", errors.New("boom"))
	f.set("b.example.com", errors.New("boom"))
	now := time.Now()
	if wait := f.beginRetry([]string{"a.example.com"}, now); wait != 0 {
		t.Fatalf("第一次应该可以重试，还要等 %v", wait)
	}
	if f.get("a.example.com") != "" {
		t.Error("重试的域名应该清掉失败记录")
	}
	if f.get("b.example.com") == "" {
		t.Error("没重试的域名不应该清掉失败记录")
	}
	f.set("a.example.com", errors.New("again"))
	if wait := f.beginRetry([]string{"a.example.com"}, now.Add(10*time.Second)); wait != 50*time.Second {
		t.Fatalf("太快了应该等 50 秒，实际 %v", wait)
	}
	if f.get("a.example.com") == "" {
		t.Error("被限流时不应该清掉失败记录")
	}
	if wait := f.beginRetry([]string{"a.example.com"}, now.Add(certRetryEvery)); wait != 0 {
		t.Fatalf("过了间隔应该可以重试，还要等 %v", wait)
	}
}
