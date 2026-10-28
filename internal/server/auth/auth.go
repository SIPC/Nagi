// Package auth 负责面板的登录：密码哈希、会话、登录限速，以及给 Connect 接口做鉴权的拦截器。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/store"
	"golang.org/x/crypto/argon2"
)

// CookieName 是会话 cookie 的名字。
const CookieName = "nagi_session"

const sessionTTL = 7 * 24 * time.Hour

// ErrBadCredentials 表示用户名或密码错误。
var ErrBadCredentials = i18n.New("用户名或密码错误")

// Service 管理用户和会话。
type Service struct {
	q       *store.Queries
	sealer  Sealer
	limiter *limiter

	mu      sync.Mutex
	pending map[int64]pendingTOTP // 正在开启两步验证、还没确认的密钥
}

// Sealer 加密两步验证的密钥（面板主密钥）。
type Sealer interface {
	Seal(plaintext, additional []byte) ([]byte, error)
	Open(ciphertext, additional []byte) ([]byte, error)
}

type pendingTOTP struct {
	secret  string
	expires time.Time
}

// New 创建登录服务。
func New(q *store.Queries, sealer Sealer) *Service {
	return &Service{q: q, sealer: sealer, limiter: newLimiter(), pending: map[int64]pendingTOTP{}}
}

// ---- 密码 ----

// argon2id 参数取 RFC 9106 推荐的第二组：64 MiB 内存、1 次迭代、4 线程。
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
)

// HashPassword 用 argon2id 计算密码哈希。
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验密码是否与哈希匹配。
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- 用户与会话 ----

// HasUsers 判断面板是否已经完成初始化（已有管理员账号）。
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	n, err := s.q.CountUsers(ctx)
	return n > 0, err
}

// CreateUser 创建用户。
func (s *Service) CreateUser(ctx context.Context, username, password string) (int64, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	return s.q.CreateUser(ctx, store.CreateUserParams{
		Username: username, PasswordHash: hash, CreatedAt: time.Now().Unix(),
	})
}

// ErrTOTPRequired 表示密码正确，但还需要两步验证码。
var ErrTOTPRequired = i18n.New("请输入两步验证码")

// Login 校验用户名、密码和两步验证码（开启了的话），成功时返回新会话的 cookie。按客户端 IP 限速。
func (s *Service) Login(ctx context.Context, ip, username, password, code string) (*http.Cookie, error) {
	key := limitKey(ip)
	done, wait := s.limiter.begin(key)
	if done == nil && wait == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("上一次登录还在处理，请稍后再试"))
	}
	if wait > 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			i18n.Errorf("登录失败次数过多，请 %d 秒后再试", int(wait.Seconds())+1))
	}
	failed := true
	defer func() { done(failed) }()
	user, err := s.q.GetUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		failed = false // 数据库出错不算登录失败
		return nil, err
	}
	hash := user.PasswordHash
	if err != nil {
		// 用户不存在时也校验一次，耗时一样，不能从响应时间判断用户名是否存在。
		hash = dummyHash()
	}
	ok, verr := verifyLimited(ctx, password, hash)
	if verr != nil {
		failed = false
		return nil, verr
	}
	if err != nil || !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrBadCredentials)
	}
	if user.TotpSecret != "" {
		if strings.TrimSpace(code) == "" {
			failed = false
			return nil, connect.NewError(connect.CodeFailedPrecondition, ErrTOTPRequired)
		}
		if err := s.useTOTP(ctx, user, code); err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
	}
	failed = false
	return s.NewSession(ctx, user.ID)
}

// verifySem 限制同时进行的密码校验数：每次校验要 64 MiB 内存，并发太多会把面板撑爆。
var verifySem = make(chan struct{}, max(2, runtime.NumCPU()))

func verifyLimited(ctx context.Context, password, hash string) (bool, error) {
	select {
	case verifySem <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-verifySem }()
	return VerifyPassword(password, hash), nil
}

var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("nagi-dummy-password")
	return h
})

// NewSession 为用户创建会话，返回要设置的 cookie。
func (s *Service) NewSession(ctx context.Context, userID int64) (*http.Cookie, error) {
	token, err := RandomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := s.q.CreateSession(ctx, store.CreateSessionParams{
		TokenHash: HashToken(token),
		UserID:    userID,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(sessionTTL).Unix(),
	}); err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  now.Add(sessionTTL),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}, nil
}

// Logout 删除会话，返回一个清除 cookie 的 cookie。
func (s *Service) Logout(ctx context.Context, token string) *http.Cookie {
	if token != "" {
		_ = s.q.DeleteSession(ctx, HashToken(token))
	}
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

// Authenticate 根据会话 token 返回用户名。
func (s *Service) Authenticate(ctx context.Context, token string) (string, error) {
	u, err := s.session(ctx, token)
	return u.Name, err
}

// User 是当前登录的用户。
type User struct {
	ID   int64
	Name string
}

func (s *Service) session(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, i18n.New("未登录")
	}
	row, err := s.q.GetSession(ctx, HashToken(token))
	if err != nil {
		return User{}, i18n.New("未登录")
	}
	if row.ExpiresAt < time.Now().Unix() {
		_ = s.q.DeleteSession(ctx, HashToken(token))
		return User{}, i18n.New("登录已过期")
	}
	return User{ID: row.UserID, Name: row.Username}, nil
}

// CleanupLoop 定期清理过期会话，直到 ctx 取消。
func (s *Service) CleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		_ = s.q.DeleteExpiredSessions(ctx, time.Now().Unix())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- 拦截器 ----

type userKey struct{}

// Username 返回当前请求的登录用户名。
func Username(ctx context.Context) string { return CurrentUser(ctx).Name }

// CurrentUser 返回当前请求的登录用户。
func CurrentUser(ctx context.Context) User {
	u, _ := ctx.Value(userKey{}).(User)
	return u
}

// Interceptor 要求请求带有有效的会话，public 中列出的接口除外。
func (s *Service) Interceptor(public ...string) connect.Interceptor {
	allowed := make(map[string]bool, len(public))
	for _, p := range public {
		allowed[p] = true
	}
	return &interceptor{s: s, public: allowed}
}

type interceptor struct {
	s      *Service
	public map[string]bool
}

func (i *interceptor) check(ctx context.Context, procedure string, h http.Header) (context.Context, error) {
	if i.public[procedure] {
		return ctx, nil
	}
	user, err := i.s.session(ctx, SessionToken(h))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return context.WithValue(ctx, userKey{}, user), nil
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.check(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.check(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// SessionToken 从请求头的 Cookie 里取出会话 token。
func SessionToken(h http.Header) string {
	for _, line := range h.Values("Cookie") {
		cookies, err := http.ParseCookie(line)
		if err != nil {
			continue
		}
		for _, c := range cookies {
			if c.Name == CookieName {
				return c.Value
			}
		}
	}
	return ""
}

// ClientIP 从 "ip:port" 形式的地址中取出 IP。
// proxySecret 是面板自带节点的 Caddy 反代面板时带的 X-Nagi-Proxy 请求头的值（见 SetProxySecret）。
var proxySecret atomic.Value

// SetProxySecret 设置 X-Nagi-Proxy 的值。
func SetProxySecret(s string) { proxySecret.Store(s) }

// RequestIP 返回客户端 IP。请求是面板自带节点的 Caddy 转发的（面板绑定了域名；来自本机，并且带着正确的 X-Nagi-Proxy）时，
// 用 X-Forwarded-For 里的第一个地址（Caddy 不采用客户端自己带的 X-Forwarded-For，写的是它看到的客户端地址）。
// 只看来源是不是本机不够：本机上的其他程序（例如 host 网络的容器）可以随便填 X-Forwarded-For，绕过登录限速。
func RequestIP(addr string, h http.Header) string {
	ip := ClientIP(addr)
	secret, _ := proxySecret.Load().(string)
	fromProxy := secret != "" && subtle.ConstantTimeCompare([]byte(h.Get("X-Nagi-Proxy")), []byte(secret)) == 1
	if p := net.ParseIP(ip); p != nil && p.IsLoopback() && fromProxy {
		if xff := h.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if fp := net.ParseIP(strings.TrimSpace(first)); fp != nil {
				return fp.String()
			}
		}
	}
	return ip
}

func ClientIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// ---- 令牌 ----

// RandomToken 生成 32 字节的随机令牌（URL 安全的 Base64）。
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken 计算令牌的哈希。数据库里只存哈希，不存令牌本身。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- 登录限速 ----

// limiter：同一来源连续失败 5 次以后，每次失败等待时间翻倍，最长 15 分钟。
// 同一来源同时只处理一个登录请求：否则并发的请求都能在失败次数增加之前通过检查，限速就没用了。
type limiter struct {
	mu    sync.Mutex
	state map[string]*attempts
	calls int
}

type attempts struct {
	failures int
	until    time.Time
	busy     bool
	last     time.Time
}

func newLimiter() *limiter { return &limiter{state: map[string]*attempts{}} }

// limitKey 是限速的来源：IPv4 按地址，IPv6 按 /64（一个用户通常有一整个 /64，可以随便换地址）。
func limitKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return ip
}

// begin 开始一次登录尝试。被锁定时返回等待时间；同一来源有请求正在处理时 done 为 nil、等待时间为 0；
// 否则返回 done，登录结束时调用，参数表示这次是否算失败。
func (l *limiter) begin(key string) (done func(failed bool), wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.calls++
	if l.calls%1000 == 0 {
		for k, a := range l.state {
			if !a.busy && now.Sub(a.last) > 24*time.Hour {
				delete(l.state, k)
			}
		}
	}
	a, ok := l.state[key]
	if !ok {
		a = &attempts{}
		l.state[key] = a
	}
	if a.busy {
		return nil, 0
	}
	if w := a.until.Sub(now); w > 0 {
		return nil, w
	}
	a.busy, a.last = true, now
	return func(failed bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		a.busy, a.last = false, time.Now()
		if !failed {
			a.failures, a.until = 0, time.Time{}
			return
		}
		a.failures++
		if a.failures >= 5 {
			a.until = time.Now().Add(min(time.Second<<min(a.failures-5, 10), 15*time.Minute))
		}
	}, 0
}
