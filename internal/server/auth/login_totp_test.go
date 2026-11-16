package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/store"
)

// plainSealer 不加密，测试用。
type plainSealer struct{}

func (plainSealer) Seal(p, _ []byte) ([]byte, error) { return p, nil }
func (plainSealer) Open(c, _ []byte) ([]byte, error) { return c, nil }

// 知道密码的人换 IP 猜两步验证码：验证码猜错按账号计数，换 IP 也会被锁。
func TestLoginTOTPLimitedPerAccount(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q := store.New(d)
	s := New(q, plainSealer{})
	id, err := s.CreateUser(ctx, "admin", "correct horse", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString([]byte("JBSWY3DPEHPK3PXP")), ID: id}); err != nil {
		t.Fatal(err)
	}
	for i := range userFreeFailures {
		_, err := s.Login(ctx, fmt.Sprintf("10.0.0.%d", i+1), "admin", "correct horse", "000000")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, "10.0.1.1", "admin", "correct horse", "000000"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("换了 IP 也应该被锁：%v", err)
	}
	// 不知道密码的人锁不住账号：密码错的尝试不碰按账号的计数。
	s2 := New(q, plainSealer{})
	for i := range userFreeFailures + 1 {
		_, _ = s2.Login(ctx, fmt.Sprintf("10.1.0.%d", i+1), "admin", "wrong", "")
	}
	if _, err := s2.Login(ctx, "10.2.0.1", "admin", "correct horse", ""); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("密码猜错不应该锁住账号：%v", err)
	}
}

// 拿到会话的人反复做错高权限确认，锁不住账号主人的登录。
func TestElevationFailuresDontLockLogin(t *testing.T) {
	l := newLimiter()
	for range userFreeFailures + 2 {
		if done, _ := l.begin(userLimitKey(1)); done != nil {
			done(outcomeFailure)
		}
	}
	if _, wait := l.begin(loginLimitKey(1)); wait > 0 {
		t.Fatal("高权限确认的失败不应该锁住登录")
	}
}
