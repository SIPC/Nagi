package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/store"
)

// 账号安全：修改密码、两步验证（TOTP）、安全入口。

func totpContext(userID int64) []byte { return []byte("totp/" + strconv.FormatInt(userID, 10)) }

func (s *Service) secret(user store.User) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(user.TotpSecret)
	if err != nil {
		return "", err
	}
	plain, err := s.sealer.Open(sealed, totpContext(user.ID))
	return string(plain), err
}

// useTOTP 校验验证码，同一个时间步只能用一次。
func (s *Service) useTOTP(ctx context.Context, user store.User, code string) error {
	secret, err := s.secret(user)
	if err != nil {
		return i18n.New("两步验证数据损坏，请在面板机器上执行 nagi reset 2fa")
	}
	step, ok := checkTOTP(secret, code, time.Now())
	if !ok {
		return i18n.New("验证码错误")
	}
	if n, err := s.q.UseTOTPStep(ctx, store.UseTOTPStepParams{TotpLastStep: step, ID: user.ID, TotpLastStep_2: step}); err != nil || n == 0 {
		return i18n.New("这个验证码已经用过了，请等下一个")
	}
	return nil
}

// TOTPEnabled 判断用户是否开启了两步验证。
func (s *Service) TOTPEnabled(ctx context.Context, userID int64) bool {
	u, err := s.q.GetUserByID(ctx, userID)
	return err == nil && u.TotpSecret != ""
}

// ChangePassword 修改密码。成功后其他地方的登录全部失效，当前会话保留。
func (s *Service) ChangePassword(ctx context.Context, userID int64, token, current, next string) error {
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(current, u.PasswordHash) {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("当前密码不对"))
	}
	if len([]rune(next)) < 8 {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("新密码至少 8 个字符"))
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.q.UpdatePassword(ctx, store.UpdatePasswordParams{PasswordHash: hash, ID: userID}); err != nil {
		return err
	}
	return s.q.DeleteOtherSessions(ctx, store.DeleteOtherSessionsParams{UserID: userID, TokenHash: HashToken(token)})
}

// BeginTOTP 生成一个新的两步验证密钥，用户在验证器 App 里添加后用 EnableTOTP 确认。10 分钟内有效。
func (s *Service) BeginTOTP(userID int64) (string, error) {
	secret, err := NewTOTPSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.pending[userID] = pendingTOTP{secret: secret, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return secret, nil
}

// EnableTOTP 用验证器 App 显示的验证码确认并开启两步验证。
// token 是当前会话：开启后其他会话全部失效（开两步验证往往是因为担心账号泄露，已经被盗的登录不能继续用）。
func (s *Service) EnableTOTP(ctx context.Context, userID int64, token, code string) error {
	s.mu.Lock()
	p, ok := s.pending[userID]
	s.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("请重新开始设置两步验证"))
	}
	step, ok := checkTOTP(p.secret, code, time.Now())
	if !ok {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("验证码错误，请检查手机时间是否准确"))
	}
	sealed, err := s.sealer.Seal([]byte(p.secret), totpContext(userID))
	if err != nil {
		return err
	}
	if err := s.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString(sealed), ID: userID}); err != nil {
		return err
	}
	// 开启时用过的这个码不能再用来登录。
	_, _ = s.q.UseTOTPStep(ctx, store.UseTOTPStepParams{TotpLastStep: step, ID: userID, TotpLastStep_2: step})
	s.mu.Lock()
	delete(s.pending, userID)
	s.mu.Unlock()
	return s.q.DeleteOtherSessions(ctx, store.DeleteOtherSessionsParams{UserID: userID, TokenHash: HashToken(token)})
}

// DisableTOTP 关闭两步验证，需要当前的验证码或者密码。
func (s *Service) DisableTOTP(ctx context.Context, userID int64, code, password string) error {
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.TotpSecret == "" {
		return nil
	}
	ok := password != "" && VerifyPassword(password, u.PasswordHash)
	if !ok && code != "" {
		ok = s.useTOTP(ctx, u, code) == nil
	}
	if !ok {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("验证码或密码不对"))
	}
	return s.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: "", ID: userID})
}

// ---- 安全入口 ----

// SettingEntrance 是安全入口路径在 settings 表里的键，没有这一项表示没开启。
const SettingEntrance = "entrance"

const entranceCookie = "nagi_entrance"

// Entrance 返回安全入口路径（不含 /），没开启时为空。
func (s *Service) Entrance(ctx context.Context) string {
	v, err := s.q.GetSetting(ctx, SettingEntrance)
	if err != nil {
		return ""
	}
	return v
}

// SetEntrance 开启（生成新的随机路径）或关闭安全入口，返回新的路径。
func (s *Service) SetEntrance(ctx context.Context, enabled bool) (string, error) {
	if !enabled {
		return "", s.q.DeleteSetting(ctx, SettingEntrance)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	path := base64.RawURLEncoding.EncodeToString(b)
	return path, s.q.SetSetting(ctx, store.SetSettingParams{Key: SettingEntrance, Value: path})
}

func entranceToken(path string) string { return HashToken("entrance/" + path)[:32] }

// EntranceMiddleware：开启安全入口后，没登录的访问者必须先打开一次 /<入口>（设置一个 cookie），
// 否则面板的页面和接口一律返回 404，看起来就像这个地址上没有面板。已登录的会话、项目 webhook、
// 节点安装脚本不受影响（节点的控制连接按 ALPN 分流，不经过这里）。
func (s *Service) EntranceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := s.Entrance(r.Context())
		if path == "" {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		if p == "/"+path || p == "/"+path+"/" {
			http.SetCookie(w, &http.Cookie{Name: entranceCookie, Value: entranceToken(path), Path: "/",
				MaxAge: 30 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if strings.HasPrefix(p, "/hooks/") || p == "/install.sh" || p == "/api/standby/pair" || p == "/api/standby/status" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(entranceCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(entranceToken(path))) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := s.session(r.Context(), SessionToken(r.Header)); err == nil {
			next.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}
