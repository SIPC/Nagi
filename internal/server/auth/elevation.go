package auth

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/store"
)

// ElevationTTL 是高权限确认的有效期（见 docs/node-access.md#权限）。
const ElevationTTL = 15 * time.Minute

// Elevated 判断这个用户当前的会话做过高权限确认，还没有过期。
func (u User) Elevated() bool { return u.ElevatedUntil > time.Now().Unix() }

// Elevate 再验证一次身份：开了两步验证的账号用验证码，没开的用密码。失败次数和登录共用限速。
// 记在会话上，只在服务端，客户端伪造不了。
func (s *Service) Elevate(ctx context.Context, ip, token, password, code string) (time.Time, error) {
	key := limitKey(ip)
	done, wait := s.limiter.begin(key)
	if done == nil && wait == 0 {
		return time.Time{}, connect.NewError(connect.CodeResourceExhausted, i18n.New("验证进行中，请稍后重试"))
	}
	if wait > 0 {
		if n := int(wait.Seconds()) + 1; n > 1 {
			return time.Time{}, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("验证失败次数过多，请 %d 秒后重试", n))
		}
		return time.Time{}, connect.NewError(connect.CodeResourceExhausted, i18n.New("验证失败次数过多，请 1 秒后重试"))
	}
	failed := true
	defer func() { done(failed) }()
	cur, err := s.session(ctx, token)
	if err != nil {
		failed = false
		return time.Time{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	user, err := s.q.GetUserByID(ctx, cur.ID)
	if err != nil {
		failed = false
		return time.Time{}, err
	}
	if user.TotpSecret != "" {
		if strings.TrimSpace(code) == "" {
			failed = false
			return time.Time{}, connect.NewError(connect.CodeFailedPrecondition, ErrTOTPRequired)
		}
		if err := s.useTOTP(ctx, user, code); err != nil {
			return time.Time{}, connect.NewError(connect.CodePermissionDenied, err)
		}
	} else {
		ok, err := verifyLimited(ctx, password, user.PasswordHash)
		if err != nil {
			failed = false
			return time.Time{}, err
		}
		if !ok {
			return time.Time{}, connect.NewError(connect.CodePermissionDenied, i18n.New("密码不正确"))
		}
	}
	failed = false
	until := time.Now().Add(ElevationTTL)
	if err := s.q.SetSessionElevated(ctx, store.SetSessionElevatedParams{ElevatedUntil: until.Unix(), TokenHash: HashToken(token)}); err != nil {
		return time.Time{}, err
	}
	return until, nil
}
