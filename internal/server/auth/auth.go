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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth/attestation"
	"github.com/SIPC/nagi/internal/server/store"
	"golang.org/x/crypto/argon2"
)

// CookieName 是会话 cookie 的名字。带 __Host- 前缀：浏览器保证它只能由面板自己的域名、通过 HTTPS 设置，
// 兄弟子域名上的网站覆盖不了（见 docs/panel.md#会话）。
const CookieName = "__Host-nagi_session"

// legacyCookieName 是 v0.29.0 以前的名字。v0.29.1 起不再接受（见 docs/panel.md#会话）：它没有 __Host- 前缀，
// 同一个主域名下的其他子域名可以给它种一个自己的会话（登录 CSRF）。收到时只让浏览器删掉。
const legacyCookieName = "nagi_session"

const sessionTTL = 7 * 24 * time.Hour

// ErrBadCredentials 表示用户名或密码错误。
var ErrBadCredentials = i18n.New("用户名或密码不正确")

// Service 管理用户和会话。
type Service struct {
	q       *store.Queries
	sealer  Sealer
	limiter *limiter

	mu      sync.Mutex
	pending map[int64]pendingTOTP // 正在开启两步验证、还没确认的密钥
	// challenges 是通行密钥的一次性挑战（见 passkey.go）。
	challenges map[string]passkeyChallenge
	// loginKey 签无状态的通行密钥登录挑战，usedLogin 按过期时间（分钟）分桶记着用过的编号（见 passkey.go）。
	loginKey  []byte
	usedLogin map[int64]map[string]bool
	// tokenAlerts：令牌新网段告警的合并（见 tokennet.go）。
	tokenAlerts tokenAlerts

	audit    *audit.Log
	failures failureSummary
	// app 是安卓 App 的配对码、挑战和令牌（见 app.go）。
	app appState
	// revocations 是取到的证明证书吊销列表，没有时用随面板发布的（见 revocation.go）。
	revocations atomic.Pointer[attestation.Revocations]
	// recheckMu：按吊销列表复查（RecheckAppDevices）同时只有一个。
	recheckMu sync.Mutex
	// recheckSignal 叫 RevocationLoop 复查（见 requestRecheck）。
	recheckSignal chan struct{}
	streaks     totpStreaks

	// Alert 发一条安全告警（会话被盗用等），由面板接到告警渠道上。key 用来合并重复的告警。
	Alert func(ctx context.Context, key, title, message string)
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
	return &Service{q: q, sealer: sealer, limiter: newLimiter(), pending: map[int64]pendingTOTP{}, recheckSignal: make(chan struct{}, 1)}
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
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (int64, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	return s.q.CreateUser(ctx, store.CreateUserParams{
		Username: username, PasswordHash: hash, CreatedAt: time.Now().Unix(), Role: role,
	})
}

// ErrTOTPRequired 表示密码正确，但还需要两步验证码。
var ErrTOTPRequired = i18n.New("请输入两步验证码")

// Login 校验用户名、密码和两步验证码（开启了的话），成功时返回新会话的 cookie。按客户端 IP 限速。
func (s *Service) Login(ctx context.Context, ip, username, password, code string) (*http.Cookie, error) {
	key := LimitKey(ip)
	done, wait := s.limiter.begin(key)
	if done == nil && wait == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("上一次登录仍在处理，请稍后重试"))
	}
	if wait > 0 {
		s.rateLimited(ip)
		if n := int(wait.Seconds()) + 1; n > 1 {
			return nil, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("登录失败次数过多，请 %d 秒后重试", n))
		}
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("登录失败次数过多，请 1 秒后重试"))
	}
	result := outcomeFailure
	defer func() { done(result) }()
	user, err := s.q.GetUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		result = outcomeNeutral // 数据库出错不算登录失败
		return nil, err
	}
	hash := user.PasswordHash
	if err != nil {
		// 用户不存在时也校验一次，耗时一样，不能从响应时间判断用户名是否存在。
		hash = dummyHash()
	}
	ok, verr := verifyLimited(ctx, password, hash)
	if verr != nil {
		result = outcomeNeutral
		return nil, verr
	}
	if err != nil || !ok {
		s.recordLoginFailure(ctx, ip, username)
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrBadCredentials)
	}
	if user.TotpSecret != "" {
		if strings.TrimSpace(code) == "" {
			// 密码对了、还没填验证码：不算失败，也不能当成功清零（否则可以借它无限次猜验证码）。
			result = outcomeNeutral
			return nil, connect.NewError(connect.CodeFailedPrecondition, ErrTOTPRequired)
		}
		// 验证码另外按账号计数：知道密码的人换 IP 也不能一直猜。只有密码对了才走到这里，
		// 不知道密码的人锁不住别人的账号。
		userDone, err := s.beginVerify(loginLimitKey(user.ID))
		if err != nil {
			result = outcomeNeutral
			return nil, err
		}
		if err := s.useTOTP(ctx, user, code); err != nil {
			userDone(outcomeFailure)
			at, aid, an := userActor(user.ID, user.Username)
			s.loginEvent(ctx, audit.Event{Action: "totp_failed", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
				Result: audit.ResultFailed, Error: err.Error()})
			s.totpFailed(ctx, user.ID, user.Username, ip)
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		userDone(outcomeSuccess)
		s.totpOK(user.ID)
	}
	// 管理员登录地址：密码和验证码都对了才判断，并告警（凭据很可能泄露了）。
	if user.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.adminNetworkDenied(ctx, user.ID, user.Username, ip, "password")
		return nil, errAdminNetwork()
	}
	result = outcomeSuccess
	cookie, err := s.NewSession(ctx, user.ID, ip)
	if err == nil {
		at, aid, an := userActor(user.ID, user.Username)
		st, _, _, _ := s.q.SessionByToken(ctx, HashToken(cookie.Value))
		s.loginEvent(ctx, audit.Event{Action: "Login", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
			Session: audit.SessionTag(st.ID)})
		s.checkNewNetwork(ctx, user.ID, user.Username, user.Role, ip, RequestMeta(ctx).UserAgent)
	}
	return cookie, err
}

// verifySem 限制同时进行的密码校验数：每次校验要 64 MiB 内存，并发太多会把面板撑爆。按可用的 CPU 算
// （GOMAXPROCS 会按容器的 CPU 限额调整，NumCPU 不会），最多 4 个，内存最多占 256 MiB。
var verifySem = make(chan struct{}, verifySlots(runtime.GOMAXPROCS(0)))

func verifySlots(procs int) int { return min(max(2, procs), 4) }

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

// NewSession 为用户创建会话，返回要设置的 cookie。浏览器标识和 TLS 指纹从 ctx 里的请求来源取（RequestMeta）。
func (s *Service) NewSession(ctx context.Context, userID int64, ip string) (*http.Cookie, error) {
	token, err := RandomToken()
	if err != nil {
		return nil, err
	}
	id, err := RandomToken()
	if err != nil {
		return nil, err
	}
	m := RequestMeta(ctx)
	now := time.Now()
	ttl := s.Policy(ctx).MaxAge
	if err := s.q.CreateSession(ctx, store.CreateSessionParams{
		TokenHash: HashToken(token),
		UserID:    userID,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		Ip:        ip,
		ID:        id[:32],
		RotatedAt: now.Unix(),
		UserAgent: m.UserAgent,
		LastIp:    ip,
		LastSeen:  now.Unix(),
		TlsFp:     m.Fingerprint,
		KeyUntil:  now.Add(keyWindow).Unix(),
	}); err != nil {
		return nil, err
	}
	return sessionCookie(token, now.Add(ttl)), nil
}

// sessionCookie 是会话的 cookie。
func sessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: token, Path: "/", Expires: expires, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

// RotatedCookie 是换发的新令牌的 cookie，有效期和会话一样。
func (s *Service) RotatedCookie(ctx context.Context, token string) *http.Cookie {
	st, _, err := s.liveSession(ctx, token)
	exp := time.Now().Add(sessionTTL)
	if err == nil {
		exp = time.Unix(st.ExpiresAt, 0)
	}
	return sessionCookie(token, exp)
}

// Logout 删除会话，返回一个清除 cookie 的 cookie。
func (s *Service) Logout(ctx context.Context, token string) *http.Cookie {
	if st, _, err := s.liveSession(ctx, token); err == nil {
		_ = s.q.DeleteSessionByID(ctx, st.ID)
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
	Role string
	// ElevatedUntil 是这个会话的高权限确认的到期时间（Unix 秒）。见 elevation.go。
	ElevatedUntil int64
	// LimitProjects、LimitNodes 表示只能访问指定的项目、节点（见 docs/panel.md#访问范围）。管理员不受限制。
	LimitProjects, LimitNodes bool
	// 用访问令牌（MCP）调用时：令牌的 ID、活动记录里的来源（mcp:<令牌名>），以及令牌是否允许修改、执行命令、
	// 在节点上执行命令（见 docs/mcp.md#权限）。网页的会话这几项为空。
	TokenID               int64
	Via                   string
	AllowWrite, AllowExec bool
	AllowNodeExec         bool
	// AllowPanel：令牌可以管理面板（只给管理员，见 docs/mcp.md#管理面板）。
	AllowPanel bool
	// SessionID 是网页会话的固定 ID（审计记录用它的哈希把一次登录里的操作串起来），令牌调用为空。
	SessionID string
	// ElevatedBy 是高权限确认用的方式（ElevatedByPassword），见 passwordElevation。
	ElevatedBy string
	// DeviceID、DeviceName：用安卓 App 的令牌调用时是这台手机（Via 是 app，见 docs/android.md）。
	DeviceID, DeviceName string
}

// SessionUser 根据会话 token 返回用户，给不走 Connect 的入口检查角色用。
func (s *Service) SessionUser(ctx context.Context, token string) (User, error) {
	return s.session(ctx, token)
}

// session 找令牌对应的会话，不做设备和网络的检查，也不轮换（安全入口判断是否已登录用）。
func (s *Service) session(ctx context.Context, token string) (User, error) {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return User{}, err
	}
	if st.Reverify != "" {
		return User{}, &ErrReverify{Reason: st.Reverify}
	}
	return userOf(st), nil
}

// RequestUser 检查不走 Connect 的请求（终端、上传下载）的会话：和接口一样检查设备和网络，但不轮换令牌。
func (s *Service) RequestUser(r *http.Request) (User, error) {
	if SessionToken(r.Header) == "" {
		if t := AppBearer(r.Header); t != "" {
			return s.AppTokenUser(r.Context(), t, RequestIP(r.RemoteAddr, r.Header))
		}
	}
	info := RequestInfo{IP: RequestIP(r.RemoteAddr, r.Header), UserAgent: r.Header.Get("User-Agent"), Fingerprint: RequestFingerprint(r.Context(), r.RemoteAddr, r.Header), CDN: ViaCDN(r.Header)}
	res, err := s.authenticate(r.Context(), SessionToken(r.Header), info)
	if err != nil {
		return User{}, err
	}
	return res.user, nil
}

// CleanupLoop 定期清理过期的会话、访问令牌和没用的 OAuth 客户端，直到 ctx 取消。
func (s *Service) CleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		_ = s.q.DeleteExpiredSessions(ctx, now.Unix())
		_ = s.q.DeleteExpiredAccessTokens(ctx, now.Unix())
		// 注册了一直没人授权的 OAuth 客户端（见 docs/mcp.md#oauth）。
		_ = s.q.DeleteStaleOAuthClients(ctx, now.Add(-24*time.Hour).Unix())
		// 空闲超时的会话（见 SessionPolicy），和换下来一周以上的旧令牌。
		if idle, err := s.q.DeleteIdleSessions(ctx, now.Add(-s.Policy(ctx).Idle).Unix()); err == nil {
			for _, st := range idle {
				at, aid, an := userActor(st.UserID, st.Username)
				s.loginEvent(ctx, audit.Event{Action: "session_expired", ActorType: at, ActorID: aid, ActorName: an, IP: st.LastIP, Session: audit.SessionTag(st.ID)})
			}
		}
		_ = s.q.PruneSessionTokens(ctx, now.Add(-7*24*time.Hour).Unix())
		s.FlushLoginFailures(ctx)
		s.cleanupAppDevices(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- 拦截器 ----

type userKey struct{}

// WithUser 把用户放进 ctx，给不走 Connect 拦截器的入口用。
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

type internalUserKey struct{}

// WithInternalUser 标记进程内的调用（MCP 调用面板自己的接口）已经按访问令牌核对过用户，登录检查直接采用它。
// 只有进程里的代码能设置 ctx，所以不能从网络伪造。
func WithInternalUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, internalUserKey{}, u)
}

// Meta 是请求的来源：客户端 IP（经本机 Caddy 转发时按 RequestIP 核对）和 User-Agent，审计记录用。
type Meta struct {
	IP, UserAgent string
	// Client 是 MCP 客户端的名称（OAuth 客户端名或 User-Agent）。
	Client string
	// Fingerprint 是 TLS 指纹（见 ja4.go），CDN 表示请求带着 CDN 的请求头。
	Fingerprint string
	CDN         bool
}

type metaKey struct{}

// WithMeta 把请求的来源放进 ctx。
func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

// RequestMeta 返回 ctx 里请求的来源。
func RequestMeta(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey{}).(Meta)
	return m
}

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

// checked 是登录检查的结果：要发给页面签名的挑战、要换发的 cookie。
type checked struct {
	challenge string
	cookies   []*http.Cookie
}

func (i *interceptor) check(ctx context.Context, procedure, addr string, h http.Header) (context.Context, checked, error) {
	// MCP 在进程内调用接口，用户已经按令牌核对过（见 WithInternalUser）。
	if u, ok := ctx.Value(internalUserKey{}).(User); ok {
		return context.WithValue(ctx, userKey{}, u), checked{}, nil
	}
	m, ok := ctx.Value(metaKey{}).(Meta)
	if !ok {
		m = Meta{IP: RequestIP(addr, h), UserAgent: h.Get("User-Agent"), Fingerprint: RequestFingerprint(ctx, addr, h), CDN: ViaCDN(h)}
		ctx = WithMeta(ctx, m)
	}
	if i.public[procedure] {
		return ctx, checked{}, nil
	}
	token := SessionToken(h)
	// 安卓 App：不带 cookie，带 Authorization: Bearer nagi_app_…（见 docs/android.md#换取令牌）。
	if app := AppBearer(h); token == "" && app != "" {
		u, err := i.s.AppTokenUser(ctx, app, m.IP)
		if err != nil {
			return nil, checked{}, appAuthError(err)
		}
		return context.WithValue(ctx, userKey{}, u), checked{}, nil
	}
	res, err := i.s.authenticate(ctx, token, RequestInfo{IP: m.IP, UserAgent: m.UserAgent, Fingerprint: m.Fingerprint, CDN: m.CDN})
	var rv *ErrReverify
	if errors.As(err, &rv) {
		return nil, checked{}, i.s.reverifyError(ctx, rv.UserID, rv.Reason)
	} else if err != nil {
		return nil, checked{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	var out checked
	out.challenge = res.challenge
	// 还带着旧名字的 cookie：让浏览器删掉（不再使用它）。
	if hasLegacyCookie(h) {
		out.cookies = append(out.cookies, &http.Cookie{Name: legacyCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	}
	return context.WithValue(ctx, userKey{}, res.user), out, nil
}

func (c checked) apply(h http.Header) {
	if c.challenge != "" {
		h.Set(ChallengeHeader, c.challenge)
	}
	for _, ck := range c.cookies {
		h.Add("Set-Cookie", ck.String())
	}
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, c, err := i.check(ctx, req.Spec().Procedure, req.Peer().Addr, req.Header())
		if err != nil {
			return nil, err
		}
		res, err := next(ctx, req)
		if res != nil {
			c.apply(res.Header())
		} else if ce := new(connect.Error); errors.As(err, &ce) {
			c.apply(ce.Meta())
		}
		return res, err
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, c, err := i.check(ctx, conn.Spec().Procedure, conn.Peer().Addr, conn.RequestHeader())
		if err != nil {
			return err
		}
		c.apply(conn.ResponseHeader())
		return next(ctx, conn)
	}
}

// SessionToken 从请求头的 Cookie 里取出会话 token。只认带 __Host- 前缀的名字。
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

// hasLegacyCookie 判断请求带着旧名字的 cookie。
func hasLegacyCookie(h http.Header) bool {
	for _, line := range h.Values("Cookie") {
		cookies, _ := http.ParseCookie(line)
		for _, c := range cookies {
			if c.Name == legacyCookieName {
				return true
			}
		}
	}
	return false
}

// fromLocalProxy 判断请求是不是面板自带节点的 Caddy 转发的：来自本机，并且带着正确的 X-Nagi-Proxy。
func fromLocalProxy(addr string, h http.Header) bool {
	secret, _ := proxySecret.Load().(string)
	if secret == "" || subtle.ConstantTimeCompare([]byte(h.Get("X-Nagi-Proxy")), []byte(secret)) != 1 {
		return false
	}
	p := net.ParseIP(ClientIP(addr))
	return p != nil && p.IsLoopback()
}

// RequestHost 返回客户端访问面板用的主机名（MCP 和 OAuth 的地址用它拼，见 docs/mcp.md#入口）。
// 本机 Caddy 用 HTTPS 连面板时会把 Host 换成上游地址（127.0.0.1:<面板端口>），原来的域名在 X-Forwarded-Host 里；
// 只有确认是它转发的请求才采用这个头，别人填的不算。
func RequestHost(r *http.Request) string {
	if fromLocalProxy(r.RemoteAddr, r.Header) {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			first, _, _ := strings.Cut(xfh, ",")
			if host := strings.TrimSpace(first); host != "" {
				return host
			}
		}
	}
	return r.Host
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
	if fromLocalProxy(addr, h) {
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

// limiter：同一来源连续失败若干次以后（账号 5 次，IP 10 次：同一出口 IP 后面可能有好几个人），每次失败等待时间翻倍，最长 15 分钟。
// 同一来源同时只处理一个请求：否则并发的请求都能在失败次数增加之前通过检查，限速就没用了。
//
// 来源有两种：按 IP（LimitKey）和按账号（userLimitKey）。成功只清零账号的计数：按 IP 的计数要是也清零，
// 同一 IP 上的人用自己的账号成功登录一次，就能清掉猜别人密码的失败次数。按 IP 的失败在最后一次失败
// failureMemory 以后淡出。
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
	lastFail time.Time
}

// failureMemory 是失败次数记多久：最后一次失败之后这么久没有再失败，计数清零。
const failureMemory = time.Hour

// 失败多少次以后开始等待：按账号 5 次；按 IP 10 次，同一出口 IP 后面的几个人合计输错几次不至于马上被锁。
const (
	userFreeFailures = 5
	ipFreeFailures   = 10
)

// outcome 是一次尝试的结果。
type outcome int

const (
	// outcomeNeutral：不算失败也不算成功，例如密码正确但还没填验证码、数据库出错。
	outcomeNeutral outcome = iota
	outcomeFailure
	outcomeSuccess
)

func newLimiter() *limiter { return &limiter{state: map[string]*attempts{}} }

// LimitKey 是限速的来源：IPv4 按地址，IPv6 按 /64（一个用户通常有一整个 /64，可以随便换地址）。
func LimitKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return ip
}

// userLimitKey 是按账号限速的来源：高权限确认、改密码、关闭两步验证都猜同一个账号的密码或验证码，共用一份计数。
func userLimitKey(userID int64) string { return "user:" + strconv.FormatInt(userID, 10) }

// loginLimitKey 是登录时验证码按账号限速的来源，和 userLimitKey 分开：拿到会话的人反复做错高权限确认，
// 不能借此锁住账号主人的登录（让他没法登录进来撤销会话）。
func loginLimitKey(userID int64) string { return "user:login:" + strconv.FormatInt(userID, 10) }

// begin 开始一次尝试。任一来源被锁定时返回等待时间（最长的那个）；任一来源有请求正在处理时 done 为 nil、
// 等待时间为 0；否则返回 done，结束时调用，参数是这次的结果。
func (l *limiter) begin(keys ...string) (done func(outcome), wait time.Duration) {
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
	list := make([]*attempts, 0, len(keys))
	for _, key := range keys {
		a, ok := l.state[key]
		if !ok {
			a = &attempts{}
			l.state[key] = a
		}
		if a.failures > 0 && now.Sub(a.lastFail) > failureMemory && !now.Before(a.until) {
			a.failures, a.until = 0, time.Time{}
		}
		if a.busy {
			return nil, 0
		}
		wait = max(wait, a.until.Sub(now))
		list = append(list, a)
	}
	if wait > 0 {
		return nil, wait
	}
	for _, a := range list {
		a.busy, a.last = true, now
	}
	return func(o outcome) {
		l.mu.Lock()
		defer l.mu.Unlock()
		now := time.Now()
		for i, a := range list {
			a.busy, a.last = false, now
			switch o {
			case outcomeSuccess:
				if strings.HasPrefix(keys[i], "user:") {
					a.failures, a.until = 0, time.Time{}
				}
			case outcomeFailure:
				a.failures++
				a.lastFail = now
				free := ipFreeFailures
				if strings.HasPrefix(keys[i], "user:") {
					free = userFreeFailures
				}
				if a.failures >= free {
					a.until = now.Add(min(time.Second<<min(a.failures-free, 10), 15*time.Minute))
				}
			}
		}
	}, 0
}

// WithRequest 把不走 Connect 的请求（终端、上传下载）的来源放进 ctx，审计记录用。
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	return WithMeta(ctx, Meta{IP: RequestIP(r.RemoteAddr, r.Header), UserAgent: r.Header.Get("User-Agent"), Fingerprint: RequestFingerprint(r.Context(), r.RemoteAddr, r.Header), CDN: ViaCDN(r.Header)})
}
