package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 通行密钥（见 docs/panel.md#通行密钥）。校验在 auth/passkey.go。

func (s *authService) BeginPasskeyLogin(ctx context.Context, _ *connect.Request[nagiv1.BeginPasskeyLoginRequest]) (*connect.Response[nagiv1.PasskeyChallenge], error) {
	id, challenge, rpID, err := s.Auth.BeginPasskeyLogin(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.PasskeyChallenge{ChallengeId: id, Challenge: challenge, RpId: rpID}), nil
}

func (s *authService) PasskeyLogin(ctx context.Context, req *connect.Request[nagiv1.PasskeyLoginRequest]) (*connect.Response[nagiv1.LoginResponse], error) {
	m := req.Msg
	cookie, err := s.Auth.PasskeyLogin(ctx, auth.RequestIP(req.Peer().Addr, req.Header()), auth.PasskeyAssertion{
		ChallengeID: m.GetChallengeId(), CredentialID: m.GetCredentialId(), ClientDataJSON: m.GetClientDataJson(),
		AuthenticatorData: m.GetAuthenticatorData(), Signature: m.GetSignature(), UserHandle: m.GetUserHandle(),
	})
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&nagiv1.LoginResponse{})
	resp.Header().Set("Set-Cookie", cookie.String())
	return resp, nil
}

func (s *settingsService) BeginPasskeyRegistration(ctx context.Context, _ *connect.Request[nagiv1.BeginPasskeyRegistrationRequest]) (*connect.Response[nagiv1.BeginPasskeyRegistrationResponse], error) {
	u := auth.CurrentUser(ctx)
	r, err := s.Auth.BeginPasskeyRegistration(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	display := u.Name
	if row, err := s.Q.GetUserByID(ctx, u.ID); err == nil && row.DisplayName != "" {
		display = row.DisplayName
	}
	return connect.NewResponse(&nagiv1.BeginPasskeyRegistrationResponse{
		ChallengeId: r.ChallengeID, Challenge: r.Challenge, RpId: r.RPID, UserId: r.UserHandle,
		UserName: u.Name, DisplayName: display, ExcludeCredentials: r.Exclude,
	}), nil
}

func (s *settingsService) AddPasskey(ctx context.Context, req *connect.Request[nagiv1.AddPasskeyRequest]) (*connect.Response[nagiv1.Security], error) {
	u := auth.CurrentUser(ctx)
	m := req.Msg
	if err := s.Auth.AddPasskey(ctx, u.ID, m.GetChallengeId(), m.GetName(), m.GetClientDataJson(), m.GetAttestationObject()); err != nil {
		return nil, err
	}
	s.Log.Info("已添加通行密钥", "user", u.Name)
	return connect.NewResponse(s.security(ctx)), nil
}

func (s *settingsService) DeletePasskey(ctx context.Context, req *connect.Request[nagiv1.DeletePasskeyRequest]) (*connect.Response[nagiv1.Security], error) {
	u := auth.CurrentUser(ctx)
	if _, err := s.Auth.DeletePasskey(ctx, u.ID, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(s.security(ctx)), nil
}

// passkeys 是安全设置里自己的通行密钥。
func (s *settingsService) passkeys(ctx context.Context, userID int64, domain string) []*nagiv1.Passkey {
	list, err := s.Auth.ListPasskeys(ctx, userID)
	if err != nil {
		return nil
	}
	out := make([]*nagiv1.Passkey, 0, len(list))
	for _, p := range list {
		x := &nagiv1.Passkey{Id: p.ID, Name: p.Name, CreatedAt: timestamppb.New(time.Unix(p.CreatedAt, 0)), OtherDomain: p.RPID != domain}
		if p.LastUsedAt > 0 {
			x.LastUsedAt = timestamppb.New(time.Unix(p.LastUsedAt, 0))
		}
		out = append(out, x)
	}
	return out
}
