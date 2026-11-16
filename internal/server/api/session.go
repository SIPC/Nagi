package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 会话加固的接口（见 docs/panel.md#会话）。

func requestInfo(ctx context.Context) auth.RequestInfo {
	m := auth.RequestMeta(ctx)
	return auth.RequestInfo{IP: m.IP, UserAgent: m.UserAgent, Fingerprint: m.Fingerprint, CDN: m.CDN}
}

func (s *authService) RegisterSessionKey(ctx context.Context, req *connect.Request[nagiv1.RegisterSessionKeyRequest]) (*connect.Response[nagiv1.RegisterSessionKeyResponse], error) {
	if err := s.Auth.RegisterSessionKey(ctx, auth.SessionToken(req.Header()), req.Msg.GetPublicKey()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RegisterSessionKeyResponse{}), nil
}

func (s *authService) RotateSession(ctx context.Context, req *connect.Request[nagiv1.RotateSessionRequest]) (*connect.Response[nagiv1.RotateSessionResponse], error) {
	next, err := s.Auth.RotateSession(ctx, auth.SessionToken(req.Header()), req.Msg.GetSignature(), requestInfo(ctx))
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&nagiv1.RotateSessionResponse{})
	resp.Header().Set("Set-Cookie", s.Auth.RotatedCookie(ctx, next).String())
	return resp, nil
}

func (s *authService) Reverify(ctx context.Context, req *connect.Request[nagiv1.ReverifyRequest]) (*connect.Response[nagiv1.ReverifyResponse], error) {
	if err := s.Auth.Reverify(ctx, auth.SessionToken(req.Header()), req.Msg.GetPassword(), req.Msg.GetTotpCode(), requestInfo(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.ReverifyResponse{}), nil
}

func (s *settingsService) GetSessionPolicy(ctx context.Context, _ *connect.Request[nagiv1.GetSessionPolicyRequest]) (*connect.Response[nagiv1.SessionPolicy], error) {
	return connect.NewResponse(sessionPolicy(s.Auth.Policy(ctx))), nil
}

func sessionPolicy(p auth.SessionPolicy) *nagiv1.SessionPolicy {
	return &nagiv1.SessionPolicy{IdleMinutes: int32(p.Idle / time.Minute), MaxHours: int32(p.MaxAge / time.Hour), Bind: p.Bind, BindAll: p.BindAll}
}

// SetSessionPolicy 保存会话设置。两个时长只能改短（不超过默认值）。
func (s *settingsService) SetSessionPolicy(ctx context.Context, req *connect.Request[nagiv1.SessionPolicy]) (*connect.Response[nagiv1.SessionPolicy], error) {
	m := req.Msg
	def := auth.DefaultSessionPolicy
	if m.GetIdleMinutes() < 5 || time.Duration(m.GetIdleMinutes())*time.Minute > def.Idle {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("空闲超时须在 5 到 %d 分钟之间", int(def.Idle/time.Minute)))
	}
	if m.GetMaxHours() < 1 || time.Duration(m.GetMaxHours())*time.Hour > def.MaxAge {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("登录有效期须在 1 到 %d 小时之间", int(def.MaxAge/time.Hour)))
	}
	if m.GetBind() != "subnet" && m.GetBind() != "ip" && m.GetBind() != "off" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("网络绑定只能是 subnet、ip 或 off"))
	}
	bindAll := "0"
	if m.GetBindAll() {
		bindAll = "1"
	}
	for k, v := range map[string]string{
		auth.SettingSessionIdle: strconv.Itoa(int(m.GetIdleMinutes())), auth.SettingSessionMaxAge: strconv.Itoa(int(m.GetMaxHours())),
		auth.SettingSessionBind: m.GetBind(), auth.SettingSessionBindAll: bindAll,
	} {
		if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: k, Value: v}); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(sessionPolicy(s.Auth.Policy(ctx))), nil
}

// ---- 请求来源检查 ----

// originExempt 是不用 cookie 的地址，不检查来源：项目 webhook、MCP、OAuth、节点安装脚本、备用面板。
func originExempt(path string) bool {
	return strings.HasPrefix(path, "/hooks/") || path == "/mcp" || strings.HasPrefix(path, "/oauth/") || path == "/install.sh" ||
		strings.HasPrefix(path, "/api/standby/") || strings.HasPrefix(path, "/.well-known/")
}

// sameOrigin 检查修改类请求的来源（见 docs/panel.md#会话）：带 Sec-Fetch-Site 时必须是 same-origin，没有时核对 Origin 和 Host。
// 都没带的（命令行工具）放行：它们不会自动带上浏览器的 cookie。
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, auth.RequestHost(r))
}

// OriginCheck 拒绝来源不是面板自己的修改类请求，记一条安全事件。
func (a *API) OriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if originExempt(r.URL.Path) || sameOrigin(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := auth.RequestIP(r.RemoteAddr, r.Header)
		ctx := auth.WithMeta(r.Context(), auth.Meta{IP: ip, UserAgent: r.Header.Get("User-Agent")})
		a.denyOrigin(ctx, ip, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, i18n.Sprintf(requestLang(r), "请求来源不是面板，已拒绝"), http.StatusForbidden)
	})
}
