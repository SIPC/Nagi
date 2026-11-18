package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 安卓 App 的接口（见 docs/android.md#appservice）。会话的规则在 auth/app.go。

// SettingPanelName 是面板名称在 settings 表里的键。没有设置时用面板机器的主机名。
const SettingPanelName = "panel_name"

// Name 返回面板名称：App 的顶部栏和「添加到 <面板名称>」用它。管理员可以在设置里改（SetPanelName），没有设置时是 DefaultName。
func (p *PanelInfo) Name(ctx context.Context) string {
	if v, err := p.Q.GetSetting(ctx, SettingPanelName); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return DefaultPanelName()
}

// DefaultPanelName 是没有设置名称时的面板名称：面板机器的主机名。
func DefaultPanelName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "Nagi"
}

// appHosts 是 App 签名里可以写的主机：对外地址:端口和绑定的域名（见 docs/android.md#换取令牌）。
func (a *API) appHosts(ctx context.Context) []string {
	hosts := []string{a.Panel.PublicHostPort(ctx)}
	if d := a.Panel.Domain(ctx); d != "" {
		hosts = append(hosts, d, net.JoinHostPort(d, "443"))
	}
	return hosts
}

// appFeatures 是面板支持的 App 功能，App 据此决定显示哪些入口（见 docs/android.md#版本兼容）。
var appFeatures = []string{"overview-stream", "traffic-history"}

// ---- 网页：生成配对码 ----

func (s *authService) CreateAppPairing(ctx context.Context, _ *connect.Request[nagiv1.CreateAppPairingRequest]) (*connect.Response[nagiv1.CreateAppPairingResponse], error) {
	if s.Panel.Standby != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("备用面板上不能添加手机"))
	}
	code, exp, err := s.Auth.CreateAppPairing(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return nil, err
	}
	uri, err := appPairingURI(s.Panel.PublicHostPort(ctx), s.Panel.Domain(ctx), s.Panel.Fingerprint, code)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.CreateAppPairingResponse{Code: code, Uri: uri, ExpiresAt: timestamppb.New(exp)}), nil
}

func (s *authService) GetAppPairing(ctx context.Context, req *connect.Request[nagiv1.GetAppPairingRequest]) (*connect.Response[nagiv1.GetAppPairingResponse], error) {
	used, name, expired := s.Auth.AppPairingStatus(ctx, auth.CurrentUser(ctx), req.Msg.GetCode())
	return connect.NewResponse(&nagiv1.GetAppPairingResponse{Used: used, DeviceName: name, Expired: expired}), nil
}

// ---- App ----

type appService struct{ *API }

func (s *appService) ip(ctx context.Context) string { return auth.RequestMeta(ctx).IP }

func (s *appService) GetPairing(ctx context.Context, req *connect.Request[nagiv1.GetPairingRequest]) (*connect.Response[nagiv1.GetPairingResponse], error) {
	u, err := s.Auth.GetAppPairing(ctx, s.ip(ctx), req.Msg.GetCode())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.GetPairingResponse{
		PanelName: s.Panel.Name(ctx), Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, PanelVersion: s.Panel.Version,
		Domain: s.Panel.Domain(ctx), Avatar: u.Avatar, AvatarType: avatarType(u.Avatar),
	}), nil
}

func appKey(k *nagiv1.AppKey) auth.AppKeyProof {
	return auth.AppKeyProof{PublicKey: k.GetPublicKey(), Attestation: k.GetAttestation()}
}

func (s *appService) Pair(ctx context.Context, req *connect.Request[nagiv1.PairRequest]) (*connect.Response[nagiv1.PairResponse], error) {
	if s.Panel.Standby != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("备用面板上不能添加手机"))
	}
	m := req.Msg
	d := m.GetDevice()
	res, err := s.Auth.PairApp(ctx, s.ip(ctx), m.GetCode(), [3]auth.AppKeyProof{appKey(m.GetDeviceKey()), appKey(m.GetConfirmKey()), appKey(m.GetPushKey())},
		auth.AppDeviceInfo{Model: d.GetModel(), AndroidVersion: d.GetAndroidVersion(), AppVersion: d.GetAppVersion(), SDK: int(d.GetSdk())})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.PairResponse{DeviceId: res.DeviceID, PanelName: s.Panel.Name(ctx)}), nil
}

func (s *appService) Challenge(ctx context.Context, req *connect.Request[nagiv1.AppChallengeRequest]) (*connect.Response[nagiv1.AppChallengeResponse], error) {
	c, err := s.Auth.AppChallenge(ctx, s.ip(ctx), req.Msg.GetDeviceId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.AppChallengeResponse{Challenge: c}), nil
}

func (s *appService) Token(ctx context.Context, req *connect.Request[nagiv1.AppTokenRequest]) (*connect.Response[nagiv1.AppTokenResponse], error) {
	m := req.Msg
	res, err := s.Auth.AppToken(ctx, s.ip(ctx), auth.AppTokenRequest{DeviceID: m.GetDeviceId(), Challenge: m.GetChallenge(), Key: m.GetKey(),
		Host: m.GetHost(), Signature: m.GetSignature()}, s.appHosts(ctx), s.Panel.Standby != nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.AppTokenResponse{Token: res.Token, ExpiresAt: timestamppb.New(res.Expires),
		NextChallenge: res.NextChallenge, UnlockMinutes: int32(res.UnlockMinutes)}), nil
}

// appUser 返回调用者，不是用 App 令牌调用时拒绝。
func appUser(ctx context.Context) (auth.User, error) {
	u := auth.CurrentUser(ctx)
	if u.DeviceID == "" {
		return u, connect.NewError(connect.CodePermissionDenied, i18n.New("只能从 App 调用"))
	}
	return u, nil
}

func (s *appService) Info(ctx context.Context, _ *connect.Request[nagiv1.AppInfoRequest]) (*connect.Response[nagiv1.AppInfoResponse], error) {
	u, err := appUser(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.AppInfoResponse{
		PanelName: s.Panel.Name(ctx), Version: s.Panel.Version, Features: appFeatures, PublicAddress: s.Panel.PublicHostPort(ctx),
		Domain: s.Panel.Domain(ctx), UnlockMinutes: int32(s.Auth.AppUnlockMinutes(ctx)), DeviceName: u.DeviceName,
		Username: u.Name, Role: u.Role,
	}
	if row, err := s.Q.GetUserByID(ctx, u.ID); err == nil {
		out.DisplayName, out.Avatar, out.AvatarType = row.DisplayName, row.Avatar, avatarType(row.Avatar)
	}
	return connect.NewResponse(out), nil
}

func (s *appService) Logout(ctx context.Context, _ *connect.Request[nagiv1.AppLogoutRequest]) (*connect.Response[nagiv1.AppLogoutResponse], error) {
	u, err := appUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.Auth.RemoveAppDevice(ctx, u.ID, u.Name, u.DeviceID, "logout"); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.AppLogoutResponse{}), nil
}

// ---- 已登录的设备里的手机 ----

// appSessionPrefix 是手机在「已登录的设备」里的 ID 前缀（后面是设备 ID），RevokeSession 据此退出手机。
const appSessionPrefix = "app-"

func (s *settingsService) appSessions(ctx context.Context, userID int64, current string) ([]*nagiv1.LoginSession, error) {
	list, err := s.Q.UserAppDevices(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.LoginSession, 0, len(list))
	for _, d := range list {
		ls := &nagiv1.LoginSession{
			Id: appSessionPrefix + d.ID, Kind: "app", Current: d.ID == current, CreatedAt: timestamppb.New(time.Unix(d.CreatedAt, 0)),
			Ip: d.LastIP, LastIp: d.LastIP, Agent: d.Name, DeviceName: d.Name, Model: d.Model, AndroidVersion: d.AndroidVersion,
			AppVersion: d.AppVersion, SecurityLevel: d.SecurityLevel, BootLocked: d.BootLocked, RevocationChecked: d.RevocationChecked,
			DevKeys: d.DevKeys, RevokedReason: d.RevokedReason,
		}
		if d.LastUsedAt > 0 {
			ls.LastSeen = timestamppb.New(time.Unix(d.LastUsedAt, 0))
			ls.ExpiresAt = timestamppb.New(time.Unix(d.LastUsedAt, 0).Add(auth.AppIdleDays * 24 * time.Hour))
		}
		out = append(out, ls)
	}
	return out, nil
}

// revokeAppSession 退出自己的一台手机。
func (s *settingsService) revokeAppSession(ctx context.Context, id string) (bool, error) {
	u := auth.CurrentUser(ctx)
	return s.Auth.RemoveAppDevice(ctx, u.ID, u.Name, strings.TrimPrefix(id, appSessionPrefix), "revoked:"+strconv.FormatInt(u.ID, 10))
}

// appPairingURI 是「添加手机」二维码的内容（见 docs/android.md#扫码）：
// nagi-app://pair?v=2&a=<对外地址:端口>&d=<域名>&k=<公钥指纹的前 128 位，base64url>&c=<配对码>。
// 内容越短二维码的格子越少、越好扫：指纹只放前 128 位（App 连上后记下完整指纹），「:」不转义（查询参数里本来就允许）。
func appPairingURI(hostPort, domain, fingerprint, code string) (string, error) {
	sum, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fingerprint, "sha256//"))
	if err != nil || len(sum) != sha256.Size {
		return "", fmt.Errorf("面板的公钥指纹格式不对：%q", fingerprint)
	}
	esc := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "%3A", ":") }
	var b strings.Builder
	b.WriteString("nagi-app://pair?v=2&a=" + esc(hostPort))
	if domain != "" {
		b.WriteString("&d=" + esc(domain))
	}
	b.WriteString("&k=" + base64.RawURLEncoding.EncodeToString(sum[:16]) + "&c=" + esc(code))
	return b.String(), nil
}
