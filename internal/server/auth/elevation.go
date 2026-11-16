package auth

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// ElevationTTL 是高权限确认的有效期（见 docs/node-access.md#权限）。
const ElevationTTL = 15 * time.Minute

// Elevated 判断这个用户当前的会话做过高权限确认，还没有过期。
func (u User) Elevated() bool { return u.ElevatedUntil > time.Now().Unix() }

// Elevate 再验证一次身份：开了两步验证的账号用验证码，没开的用密码。按 IP 和按账号限速，失败次数按 IP 和登录共用，
// 按账号和改密码、关闭两步验证共用（拿到会话的人不能借这几个接口无限次地猜）。
// 记在会话上，只在服务端，客户端伪造不了。
func (s *Service) Elevate(ctx context.Context, ip, token, password, code string) (time.Time, error) {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return time.Time{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	cur := userOf(st)
	done, err := s.beginVerify(LimitKey(ip), userLimitKey(cur.ID))
	if err != nil {
		return time.Time{}, err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	user, err := s.q.GetUserByID(ctx, cur.ID)
	if err != nil {
		result = outcomeNeutral
		return time.Time{}, err
	}
	if user.TotpSecret != "" {
		if strings.TrimSpace(code) == "" {
			result = outcomeNeutral
			return time.Time{}, connect.NewError(connect.CodeFailedPrecondition, ErrTOTPRequired)
		}
		if err := s.useTOTP(ctx, user, code); err != nil {
			return time.Time{}, connect.NewError(connect.CodePermissionDenied, err)
		}
	} else {
		ok, err := verifyLimited(ctx, password, user.PasswordHash)
		if err != nil {
			result = outcomeNeutral
			return time.Time{}, err
		}
		if !ok {
			return time.Time{}, connect.NewError(connect.CodePermissionDenied, i18n.New("密码不正确"))
		}
	}
	result = outcomeSuccess
	until := time.Now().Add(ElevationTTL)
	// 只对做确认时的 IP 有效：15 分钟内 IP 变了要重新确认（见 docs/panel.md#会话）。
	if err := s.q.UpdateSession(ctx, st.ID, map[string]any{"elevated_until": until.Unix(), "elevated_ip": ip}); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// beginVerify 按 keys 开始一次身份验证（高权限确认、改密码、关闭两步验证），被限速时返回说明。
func (s *Service) beginVerify(keys ...string) (func(outcome), error) {
	done, wait := s.limiter.begin(keys...)
	if done == nil && wait == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("验证进行中，请稍后重试"))
	}
	if wait > 0 {
		if n := int(wait.Seconds()) + 1; n > 1 {
			return nil, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("验证失败次数过多，请 %d 秒后重试", n))
		}
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("验证失败次数过多，请 1 秒后重试"))
	}
	return done, nil
}
