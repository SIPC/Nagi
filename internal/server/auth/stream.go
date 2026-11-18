package auth

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// StreamUser 给长时间打开的流（首页的订阅流，见 docs/android.md#订阅流）每一轮重查调用者，返回成员当前的角色和访问范围。
// 拦截器只在打开流时查一次，之后降低角色、缩小访问范围、令牌过期都不会反映到 ctx 里的用户上。
//
//   - App（u.DeviceID 不为空）：按请求头里的 App 令牌重查，和拦截器一样：令牌还在、没过期、网段没变，设备记录还在，
//     管理员还要从允许的地址来。令牌过期（10 分钟）或网段变了返回带 Nagi-App-Retoken 的 unauthenticated，
//     App 换取新令牌后重新打开流，所以一条流最长用到令牌过期。
//   - 网页（u.SessionID 不为空）：按会话 ID 重查：会话还在、属于这个成员、没有过期或闲置太久、不用重新验证，
//     管理员还要从允许的地址来（同 LinkUser）。不做设备指纹的检查（连接没变），也不轮换。
//
// 其他（进程内的调用）原样返回。返回的用户不带高权限确认：流只读。
func (s *Service) StreamUser(ctx context.Context, u User, h http.Header) (User, error) {
	ip := RequestMeta(ctx).IP
	switch {
	case u.DeviceID != "":
		cur, err := s.AppTokenUser(ctx, AppBearer(h), ip)
		if err != nil {
			return User{}, appAuthError(err)
		}
		if cur.DeviceID != u.DeviceID {
			return User{}, connect.NewError(connect.CodeUnauthenticated, i18n.New("请先登录"))
		}
		return cur, nil
	case u.SessionID != "":
		cur, err := s.LinkUser(ctx, u.SessionID, u.ID, ip)
		var rv *ErrReverify
		if errors.As(err, &rv) {
			return User{}, s.reverifyError(ctx, u.ID, rv.Reason)
		} else if err != nil {
			return User{}, connect.NewError(connect.CodeUnauthenticated, err)
		}
		cur.ElevatedUntil, cur.ElevatedBy = 0, ""
		return cur, nil
	}
	return u, nil
}

// appAuthError 把 App 令牌的核对结果转成接口的错误：要重新换取令牌的带上 Nagi-App-Retoken（App 自动处理），
// 其余一律 unauthenticated。拦截器和 StreamUser 共用。
func appAuthError(err error) error {
	if rt := new(ErrAppRetoken); errors.As(err, &rt) {
		e := connect.NewError(connect.CodeUnauthenticated, err)
		e.Meta().Set(RetokenHeader, rt.Reason)
		return e
	}
	return connect.NewError(connect.CodeUnauthenticated, err)
}
