package api

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/oauth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 访问令牌和 OAuth 授权页（见 docs/mcp.md#登录）。

func accessTokenInfo(t store.AccessToken) *nagiv1.AccessToken {
	out := &nagiv1.AccessToken{
		Id: t.ID, Name: t.Name, Source: t.Source, AllowWrite: t.AllowWrite, AllowExec: t.AllowExec, AllowNodeExec: t.AllowNodeExec,
		CreatedAt: timestamppb.New(time.Unix(t.CreatedAt, 0)), LastIp: t.LastIP,
	}
	// OAuth 的访问令牌 1 小时就换，显示的是整个授权的有效期。
	expires := t.ExpiresAt
	if t.Source == auth.TokenSourceOAuth {
		expires = t.RefreshExpiresAt
	}
	if expires > 0 {
		out.ExpiresAt = timestamppb.New(time.Unix(expires, 0))
	}
	if t.LastUsedAt > 0 {
		out.LastUsedAt = timestamppb.New(time.Unix(t.LastUsedAt, 0))
	}
	return out
}

// tokenOwner 返回要查看的令牌属于谁：0 是自己；看别人的只有管理员可以。
func tokenOwner(ctx context.Context, id int64) (int64, error) {
	u := auth.CurrentUser(ctx)
	if id == 0 || id == u.ID {
		return u.ID, nil
	}
	if u.Role != auth.RoleAdmin {
		return 0, permissionDenied()
	}
	return id, nil
}

func (s *settingsService) ListAccessTokens(ctx context.Context, req *connect.Request[nagiv1.ListAccessTokensRequest]) (*connect.Response[nagiv1.ListAccessTokensResponse], error) {
	owner, err := tokenOwner(ctx, req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	rows, err := s.Q.ListAccessTokens(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListAccessTokensResponse{}
	for _, t := range rows {
		out.Tokens = append(out.Tokens, accessTokenInfo(t))
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) CreateAccessToken(ctx context.Context, req *connect.Request[nagiv1.CreateAccessTokenRequest]) (*connect.Response[nagiv1.CreateAccessTokenResponse], error) {
	u := auth.CurrentUser(ctx)
	m := req.Msg
	name := strings.TrimSpace(m.GetName())
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("令牌名称不能为空，且不超过 64 个字符"))
	}
	if m.GetDays() > 3650 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("有效期不能超过 10 年"))
	}
	if n, err := s.Q.CountAccessTokens(ctx, u.ID); err != nil {
		return nil, err
	} else if n >= oauth.MaxTokens {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("访问令牌已达上限（%d 个），请先吊销不用的", oauth.MaxTokens))
	}
	viewer := u.Role == auth.RoleViewer
	now := time.Now()
	secret := auth.NewSecret()
	t := store.AccessToken{
		UserID: u.ID, Name: name, Source: auth.TokenSourcePersonal, TokenHash: auth.HashToken(secret),
		AllowWrite: m.GetAllowWrite() && !viewer, AllowExec: m.GetAllowExec() && !viewer, CreatedAt: now.Unix(),
	}
	// 节点终端等于节点的 root：只有管理员能给令牌勾上（创建令牌本身已经要求确认过身份）。
	if m.GetAllowNodeExec() {
		if u.Role != auth.RoleAdmin {
			return nil, connect.NewError(connect.CodePermissionDenied, i18n.New("只有管理员能创建可以在节点上执行命令的令牌"))
		}
		if !t.AllowExec {
			return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("节点终端需要同时允许执行命令"))
		}
		t.AllowNodeExec = true
	}
	if m.GetDays() > 0 {
		t.ExpiresAt = now.Add(time.Duration(m.GetDays()) * 24 * time.Hour).Unix()
	}
	id, err := s.Q.InsertAccessToken(ctx, t)
	if err != nil {
		return nil, err
	}
	t.ID = id
	return connect.NewResponse(&nagiv1.CreateAccessTokenResponse{Token: accessTokenInfo(t), Secret: secret}), nil
}

func (s *settingsService) RevokeAccessToken(ctx context.Context, req *connect.Request[nagiv1.RevokeAccessTokenRequest]) (*connect.Response[nagiv1.RevokeAccessTokenResponse], error) {
	u := auth.CurrentUser(ctx)
	// 先确认令牌属于自己，或者当前是管理员；别人的令牌对非管理员当作不存在。
	owned := false
	if rows, err := s.Q.ListAccessTokens(ctx, u.ID); err == nil {
		for _, t := range rows {
			owned = owned || t.ID == req.Msg.GetId()
		}
	}
	if !owned && u.Role != auth.RoleAdmin {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("令牌不存在"))
	}
	n, err := s.Q.DeleteAccessToken(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("令牌不存在"))
	}
	return connect.NewResponse(&nagiv1.RevokeAccessTokenResponse{}), nil
}

// ---- OAuth 授权页 ----

func (s *authService) GetOAuthRequest(ctx context.Context, req *connect.Request[nagiv1.OAuthRequest]) (*connect.Response[nagiv1.GetOAuthRequestResponse], error) {
	c, err := s.OAuth.Check(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&nagiv1.GetOAuthRequestResponse{
		ClientName: c.Name, RedirectHost: oauth.RedirectHost(req.Msg.GetRedirectUri()), Role: auth.CurrentUser(ctx).Role,
	}), nil
}

func (s *authService) ApproveOAuth(ctx context.Context, req *connect.Request[nagiv1.ApproveOAuthRequest]) (*connect.Response[nagiv1.ApproveOAuthResponse], error) {
	m := req.Msg
	if m.GetDays() > 3650 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("有效期不能超过 10 年"))
	}
	redirect, err := s.OAuth.Approve(ctx, auth.CurrentUser(ctx), m.GetRequest(), s.Auth.IssuerOf(ctx), m.GetAllow(), m.GetDays(), m.GetAllowWrite(), m.GetAllowExec())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&nagiv1.ApproveOAuthResponse{RedirectUrl: redirect}), nil
}
