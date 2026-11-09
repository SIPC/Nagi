package oauth

import (
	"fmt"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/auth"
)

// 注册限速：同一个 /64 共用限额，过期的来源会被清掉，来源数有上限。
func TestAllowRegister(t *testing.T) {
	s := New(nil, nil)
	now := time.Now()
	for i := range registerPerHour {
		ip := fmt.Sprintf("2001:db8::%x", i+1)
		if !s.allowRegister(auth.LimitKey(ip), now) {
			t.Fatalf("第 %d 次注册不应被拒绝", i+1)
		}
	}
	if s.allowRegister(auth.LimitKey("2001:db8::ffff"), now) {
		t.Fatal("同一个 /64 换地址也应该算在一起")
	}
	if !s.allowRegister(auth.LimitKey("2001:db8:1::1"), now) {
		t.Fatal("别的 /64 不受影响")
	}

	// 一个小时以后，记录过期，清理时删掉来源。
	later := now.Add(time.Hour + time.Minute)
	if !s.allowRegister("1.2.3.4", later) {
		t.Fatal("新来源应该放行")
	}
	if len(s.register) != 1 {
		t.Fatalf("过期的来源应该删掉，还剩 %d 个", len(s.register))
	}

	// 来源数到了上限：新来源拒绝，已有的来源照常。
	for i := len(s.register); i < maxRegisterSources; i++ {
		s.register[fmt.Sprintf("k%d", i)] = []time.Time{later}
	}
	if s.allowRegister("5.6.7.8", later) {
		t.Fatal("来源数到了上限，新来源应该拒绝")
	}
	if !s.allowRegister("1.2.3.4", later) {
		t.Fatal("已有的来源不受上限影响")
	}
	if len(s.register) != maxRegisterSources {
		t.Fatalf("来源数超过了上限：%d", len(s.register))
	}
}
