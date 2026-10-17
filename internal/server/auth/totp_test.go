package auth

import (
	"testing"
	"time"
)

// RFC 6238 附录 B 的测试向量（SHA1，取后 6 位）。
func TestTOTPVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(key, unix/30); got != want {
			t.Errorf("t=%d: got %s want %s", unix, got, want)
		}
	}
	secret := b32.EncodeToString(key)
	now := time.Unix(1111111109, 0)
	if s, ok := checkTOTP(secret, "081804", now); !ok || s != 1111111109/30 {
		t.Fatal("当前步应该通过")
	}
	if _, ok := checkTOTP(secret, "081804", now.Add(30*time.Second)); !ok {
		t.Fatal("允许一步误差")
	}
	if _, ok := checkTOTP(secret, "081804", now.Add(90*time.Second)); ok {
		t.Fatal("超过一步不应该通过")
	}
}
