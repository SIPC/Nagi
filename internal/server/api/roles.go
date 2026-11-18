package api

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// roleCheck 按权限拦截接口（见 auth.Allowed）。回复的清理在 scopeCheck 里（clean.go）。放在登录检查之后。
type roleCheck struct{}

func permissionDenied() error {
	return connect.NewError(connect.CodePermissionDenied, i18n.New("当前账号没有权限执行这个操作"))
}

// roleAllowed：公开接口（登录、初始化）没有当前用户，由登录检查决定放不放行；没登记的接口一律拒绝。
func roleAllowed(ctx context.Context, procedure string) bool {
	u := auth.CurrentUser(ctx)
	if u.ID == 0 {
		return auth.ProcedureAccess(procedure) != ""
	}
	if u.TokenID != 0 && !auth.TokenAllowed(u, procedure) {
		return false
	}
	return auth.Allowed(u, procedure)
}

func (roleCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !roleAllowed(ctx, req.Spec().Procedure) {
			return nil, permissionDenied()
		}
		return next(ctx, req)
	}
}

func (roleCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (roleCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !roleAllowed(ctx, conn.Spec().Procedure) {
			return permissionDenied()
		}
		return next(ctx, conn)
	}
}

// ---- 会话（只能管自己的） ----

// sessionIDLen 是给界面用的会话标识的长度：令牌哈希的开头一段，不足以当作凭据。
const sessionIDLen = 16

func (s *settingsService) ListSessions(ctx context.Context, req *connect.Request[nagiv1.ListSessionsRequest]) (*connect.Response[nagiv1.ListSessionsResponse], error) {
	rows, err := s.Q.ListUserSessions(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	current := auth.CurrentUser(ctx).SessionID
	out := &nagiv1.ListSessionsResponse{}
	for _, r := range rows {
		s := &nagiv1.LoginSession{
			Id: r.ID[:min(sessionIDLen, len(r.ID))], Ip: r.Ip, Current: r.ID == current,
			CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0)), ExpiresAt: timestamppb.New(time.Unix(r.ExpiresAt, 0)),
			Agent: auth.ParseAgent(r.UserAgent).String(), LastIp: r.LastIp,
		}
		s.Kind = "web"
		if r.LastSeen > 0 {
			s.LastSeen = timestamppb.New(time.Unix(r.LastSeen, 0))
		}
		out.Sessions = append(out.Sessions, s)
	}
	phones, err := s.appSessions(ctx, auth.CurrentUser(ctx).ID, auth.CurrentUser(ctx).DeviceID)
	if err != nil {
		return nil, err
	}
	out.Sessions = append(out.Sessions, phones...)
	return connect.NewResponse(out), nil
}

func (s *settingsService) RevokeSession(ctx context.Context, req *connect.Request[nagiv1.RevokeSessionRequest]) (*connect.Response[nagiv1.RevokeSessionResponse], error) {
	if strings.HasPrefix(req.Msg.GetId(), appSessionPrefix) {
		ok, err := s.revokeAppSession(ctx, req.Msg.GetId())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, i18n.New("登录设备不存在"))
		}
		return connect.NewResponse(&nagiv1.RevokeSessionResponse{}), nil
	}
	n, err := s.Q.DeleteUserSessionByPrefix(ctx, auth.CurrentUser(ctx).ID, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("登录设备不存在"))
	}
	s.revokeDirectLinks()
	return connect.NewResponse(&nagiv1.RevokeSessionResponse{}), nil
}
