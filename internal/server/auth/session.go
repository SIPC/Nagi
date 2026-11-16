package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

// 会话加固（见 docs/panel.md#会话）：令牌轮换和重用检测、会话密钥、TLS 指纹、设备和网络检测、空闲超时。
// 信号明确时直接退出会话；信号可能来自正常使用时，要求重新验证（ErrReverify）。

const (
	// RotateInterval 是令牌轮换的间隔。
	RotateInterval = 15 * time.Minute
	// reuseGrace：旧令牌作废以后这么久之内还能用，给作废那一刻已经发出的并发请求。
	reuseGrace = 60 * time.Second
	// challengeGrace：发出挑战以后这么久还没拿到签名，要求重新验证。
	challengeGrace = 2 * time.Minute
	// keyWindow：登录或重新验证以后，这么久之内可以登记会话密钥。
	keyWindow = 2 * time.Minute
	// flipWindow：这么久之内在两个网段之间来回切换，要求重新验证。
	flipWindow = 5 * time.Minute
	// touchInterval：最近活动时间最多这么久写一次。
	touchInterval = time.Minute
	// reverifyLimit：重新验证连续失败这么多次，退出会话。
	reverifyLimit = 3
)

// ChallengeHeader 是响应里要求页面用会话密钥签名的挑战。
const ChallengeHeader = "Nagi-Session-Challenge"

// ReverifyHeader 是要求重新验证时，响应里写的原因；ReverifyMethodHeader 是验证方式（totp 或 password）。
const (
	ReverifyHeader       = "Nagi-Reverify"
	ReverifyMethodHeader = "Nagi-Reverify-Method"
)

// 重新验证的原因。
const (
	ReasonNoKey          = "session_key"       // 拿不出会话密钥的签名
	ReasonTLSChanged     = "tls_changed"       // 浏览器标识没变、TLS 指纹变了
	ReasonNetworkFlip    = "network_flip"      // 5 分钟内在两个网段之间来回切换
	ReasonNetworkChanged = "network_changed"   // 管理员的网段（或 IP）变了
	ReasonSignature      = "signature_invalid" // 签名不对
)

// ErrReverify 表示这个会话要先重新验证。
type ErrReverify struct {
	Reason string
	UserID int64
}

func (e *ErrReverify) Error() string { return i18n.New("请重新验证身份").Error() }

// errLoggedOut 是会话被退出时给请求的说明。
var errLoggedOut = i18n.New("登录已失效，请重新登录")

// SessionPolicy 是会话的设置（「设置 → 安全 → 会话」）。
type SessionPolicy struct {
	Idle   time.Duration // 空闲超时
	MaxAge time.Duration // 从登录起最长
	// Bind 是管理员的网络绑定：subnet（网段变了要重新验证）、ip（IP 变了就要）、off。
	Bind string
	// BindAll 把绑定用到所有角色。
	BindAll bool
}

// 设置项的键。
const (
	SettingSessionIdle    = "session_idle_minutes"
	SettingSessionMaxAge  = "session_max_hours"
	SettingSessionBind    = "session_bind"
	SettingSessionBindAll = "session_bind_all"
)

// DefaultSessionPolicy 是默认的会话设置。
var DefaultSessionPolicy = SessionPolicy{Idle: 24 * time.Hour, MaxAge: sessionTTL, Bind: "subnet"}

// Policy 读出会话设置。两个时长只能比默认的短。
func (s *Service) Policy(ctx context.Context) SessionPolicy {
	p := DefaultSessionPolicy
	if v, err := s.q.GetSetting(ctx, SettingSessionIdle); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && time.Duration(n)*time.Minute < p.Idle {
			p.Idle = time.Duration(n) * time.Minute
		}
	}
	if v, err := s.q.GetSetting(ctx, SettingSessionMaxAge); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && time.Duration(n)*time.Hour < p.MaxAge {
			p.MaxAge = time.Duration(n) * time.Hour
		}
	}
	if v, err := s.q.GetSetting(ctx, SettingSessionBind); err == nil && (v == "subnet" || v == "ip" || v == "off") {
		p.Bind = v
	}
	if v, err := s.q.GetSetting(ctx, SettingSessionBindAll); err == nil {
		p.BindAll = v == "1"
	}
	return p
}

// RequestInfo 是检查会话用的请求来源。
type RequestInfo struct {
	IP, UserAgent, Fingerprint string
}

// sessionResult 是检查会话的结果。
type sessionResult struct {
	user User
	// challenge 不为空时要在响应里发给页面签名。
	challenge string
	// setCookie：请求用的是旧名字的 cookie，响应里换发新名字（见 cookie.go）。
	state store.SessionState
}

// nets 是会话记下的网段。
type nets struct {
	Fam      map[string]*famNets `json:"f,omitempty"`
	Verified map[string]string   `json:"v,omitempty"` // 每种地址验证过的网段（或 IP，绑定 IP 时）
}

type famNets struct {
	Cur  string `json:"c"`
	Prev string `json:"p,omitempty"`
	Left int64  `json:"l,omitempty"` // 离开 Prev 的时间
}

// Network 返回 IP 所在的网段：IPv4 按 /24，IPv6 按 /48；fam 是 "4" 或 "6"。
func Network(ip string) (fam, network string) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", ""
	}
	a = a.Unmap()
	if a.Is4() {
		p, _ := a.Prefix(24)
		return "4", p.String()
	}
	p, _ := a.Prefix(48)
	return "6", p.String()
}

// authenticate 检查一次请求的会话：令牌、设备和网络、轮换。会话被退出时返回 errLoggedOut，要重新验证时返回 *ErrReverify。
func (s *Service) authenticate(ctx context.Context, token string, info RequestInfo) (sessionResult, error) {
	if token == "" {
		return sessionResult{}, i18n.New("请先登录")
	}
	hash := HashToken(token)
	st, state, retiredAt, err := s.q.SessionByToken(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionResult{}, i18n.New("请先登录")
	} else if err != nil {
		return sessionResult{}, err
	}
	now := time.Now()
	policy := s.Policy(ctx)
	if st.ExpiresAt < now.Unix() || now.Sub(time.Unix(st.CreatedAt, 0)) > policy.MaxAge || st.LastSeen > 0 && now.Sub(time.Unix(st.LastSeen, 0)) > policy.Idle {
		s.endSession(ctx, st, info, "session_expired", "", false)
		return sessionResult{}, i18n.New("登录已过期，请重新登录")
	}
	// 管理员登录地址：离开允许的地址就不能用，回来以后照常（会话不退出）。
	if st.Role == RoleAdmin && !s.adminAllowed(ctx, info.IP) {
		return sessionResult{}, i18n.New("管理员只能从允许的地址登录")
	}
	switch state {
	case store.TokenRetired:
		if now.Sub(time.Unix(retiredAt, 0)) <= reuseGrace {
			break
		}
		// 已经换掉的令牌又被使用：会话被复制了。
		s.endSession(ctx, st, info, "session_reuse", i18n.Errorf("账号 %s 的登录可能被盗用", st.Username).Error(), true)
		return sessionResult{}, errLoggedOut
	case store.TokenPending:
		// 浏览器带着新令牌来了：新令牌转正，旧令牌作废。
		if _, err := s.q.ConfirmToken(ctx, st.ID, st.TokenHash, hash, now.Unix()); err != nil {
			return sessionResult{}, err
		}
		st.TokenHash, st.RotatedAt, st.Challenge, st.ChallengeAt = hash, now.Unix(), "", 0
	}
	set := map[string]any{}
	reverify := st.Reverify

	// 浏览器或操作系统变了：退出。
	was, cur := ParseAgent(st.UserAgent), ParseAgent(info.UserAgent)
	if st.UserAgent != "" && (was.Browser != cur.Browser || was.OS != cur.OS) {
		s.endSession(ctx, st, info, "session_device_changed", i18n.Errorf("账号 %s 的登录在另一种浏览器或系统上使用（%s → %s），已退出", st.Username, was.String(), cur.String()).Error(), true)
		return sessionResult{}, errLoggedOut
	}
	// TLS 指纹：标识是浏览器、握手却来自脚本工具，退出；标识没变、指纹变了，重新验证。
	if fp := info.Fingerprint; fp != "" {
		switch {
		case cur.IsBrowser() && looksLikeTool(fp):
			s.endSession(ctx, st, info, "session_tls_tool", i18n.Errorf("账号 %s 的登录被脚本工具使用，已退出", st.Username).Error(), true)
			return sessionResult{}, errLoggedOut
		case st.TLSFP == "" || info.UserAgent != st.UserAgent:
			// 第一次看到指纹，或者浏览器更新了（版本号和指纹一起变）。
			set["tls_fp"] = fp
		case !sameClient(st.TLSFP, fp) && reverify == "":
			reverify = ReasonTLSChanged
			s.securityEvent(ctx, st, info, "session_tls_changed", "", nil)
		}
	}
	if info.UserAgent != st.UserAgent {
		set["user_agent"] = info.UserAgent
	}

	// 网络。
	var n nets
	_ = json.Unmarshal([]byte(st.Nets), &n)
	if n.Fam == nil {
		n.Fam = map[string]*famNets{}
	}
	if n.Verified == nil {
		n.Verified = map[string]string{}
	}
	if fam, network := Network(info.IP); fam != "" {
		f := n.Fam[fam]
		if f == nil {
			f = &famNets{Cur: network}
			n.Fam[fam] = f
		}
		if network != f.Cur {
			// 只有同一种地址的网段之间来回切换才算：浏览器会在 IPv4 和 IPv6 之间自动切换。
			if network == f.Prev && now.Sub(time.Unix(f.Left, 0)) < flipWindow && reverify == "" {
				reverify = ReasonNetworkFlip
				s.securityEvent(ctx, st, info, "session_network_flip", i18n.Errorf("账号 %s 的登录在 5 分钟内在两个网段之间来回切换（%s、%s）", st.Username, f.Cur, network).Error(), nil)
			}
			f.Prev, f.Left, f.Cur = f.Cur, now.Unix(), network
		}
		bind := "off"
		if st.Role == RoleAdmin || policy.BindAll {
			bind = policy.Bind
		}
		want := network
		if bind == "ip" {
			want = info.IP
		}
		if bind != "off" {
			if v := n.Verified[fam]; v == "" {
				n.Verified[fam] = want
			} else if v != want && reverify == "" {
				reverify = ReasonNetworkChanged
				s.securityEvent(ctx, st, info, "session_network_changed", "", map[string]string{"from": v, "to": want})
			}
		}
		if b, err := json.Marshal(n); err == nil && string(b) != st.Nets {
			set["nets"] = string(b)
		}
	}

	// 令牌轮换：到时间后发一个挑战，页面用会话密钥签名后换发新令牌（RotateSession）。
	challenge := ""
	if reverify == "" && now.Sub(time.Unix(st.RotatedAt, 0)) >= RotateInterval {
		switch {
		case len(st.PublicKey) == 0 && now.Unix() <= st.KeyUntil:
			// 刚登录，页面还没登记会话密钥。
		case len(st.PublicKey) == 0:
			reverify = ReasonNoKey
			s.securityEvent(ctx, st, info, "session_key_missing", i18n.Errorf("账号 %s 的登录拿不出会话密钥的签名，已要求重新验证", st.Username).Error(), nil)
		case st.Challenge == "":
			challenge = randomChallenge()
			set["challenge"], set["challenge_at"] = challenge, now.Unix()
		case now.Sub(time.Unix(st.ChallengeAt, 0)) > challengeGrace:
			reverify = ReasonNoKey
			set["challenge"], set["challenge_at"] = "", int64(0)
			s.securityEvent(ctx, st, info, "session_key_missing", i18n.Errorf("账号 %s 的登录拿不出会话密钥的签名，已要求重新验证", st.Username).Error(), nil)
		default:
			challenge = st.Challenge
		}
	}
	if reverify != st.Reverify {
		set["reverify"] = reverify
	}
	if info.IP != st.LastIP || now.Sub(time.Unix(st.LastSeen, 0)) >= touchInterval {
		set["last_ip"], set["last_seen"] = info.IP, now.Unix()
	}
	if err := s.q.UpdateSession(ctx, st.ID, set); err != nil {
		return sessionResult{}, err
	}
	if reverify != "" {
		return sessionResult{state: st}, &ErrReverify{Reason: reverify, UserID: st.UserID}
	}
	u := userOf(st)
	// 高权限确认只对做确认时的 IP 有效。
	if st.ElevatedIP != "" && st.ElevatedIP != info.IP {
		u.ElevatedUntil = 0
	}
	return sessionResult{user: u, challenge: challenge, state: st}, nil
}

func userOf(st store.SessionState) User {
	return User{ID: st.UserID, Name: st.Username, Role: st.Role, ElevatedUntil: st.ElevatedUntil,
		LimitProjects: st.AllProjects == 0, LimitNodes: st.AllNodes == 0, SessionID: st.ID}
}

func randomChallenge() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// endSession 退出会话并记一条审计记录；alertMsg 不为空时同时告警。
func (s *Service) endSession(ctx context.Context, st store.SessionState, info RequestInfo, action, alertMsg string, security bool) {
	_ = s.q.DeleteSessionByID(ctx, st.ID)
	if security {
		s.securityEvent(ctx, st, info, action, alertMsg, nil)
		return
	}
	at, aid, an := userActor(st.UserID, st.Username)
	s.loginEvent(ctx, audit.Event{Action: action, ActorType: at, ActorID: aid, ActorName: an, IP: info.IP, Session: audit.SessionTag(st.ID)})
}

// securityEvent 记一条安全事件（类别 security）；alertMsg 不为空时发告警。
func (s *Service) securityEvent(ctx context.Context, st store.SessionState, info RequestInfo, action, alertMsg string, params map[string]string) {
	if params == nil {
		params = map[string]string{}
	}
	if info.UserAgent != "" {
		params["user_agent"] = info.UserAgent
	}
	if info.Fingerprint != "" {
		params["tls"] = info.Fingerprint
	}
	b, _ := json.Marshal(params)
	at, aid, an := userActor(st.UserID, st.Username)
	result := audit.ResultDenied
	s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: action, ActorType: at, ActorID: aid, ActorName: an, IP: info.IP,
		Session: audit.SessionTag(st.ID), Params: string(b), Result: result})
	if alertMsg != "" && s.Alert != nil {
		msg := alertMsg
		if info.IP != "" {
			msg += "\n" + i18n.Errorf("来源 IP：%s", info.IP).Error()
		}
		s.Alert(context.WithoutCancel(ctx), "session:"+st.ID+":"+action, i18n.New("登录安全").Error(), msg)
	}
}

// ---- 会话密钥和轮换 ----

// RegisterSessionKey 登记会话密钥的公钥（SPKI，ECDSA P-256）。只能在登录或重新验证后的一小段时间内、还没有公钥时登记。
func (s *Service) RegisterSessionKey(ctx context.Context, token string, spki []byte) error {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	if len(st.PublicKey) > 0 || time.Now().Unix() > st.KeyUntil {
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("这个会话已经登记过会话密钥"))
	}
	if _, err := parseP256(spki); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("会话密钥格式不正确"))
	}
	return s.q.UpdateSession(ctx, st.ID, map[string]any{"public_key": spki, "key_until": int64(0)})
}

func parseP256(spki []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(*ecdsa.PublicKey)
	if !ok || pk.Curve != elliptic.P256() {
		return nil, errors.New("not a P-256 key")
	}
	return pk, nil
}

// verifyP1363 校验 WebCrypto 的 ECDSA 签名（r‖s 各 32 字节，SHA-256）。
func verifyP1363(pk *ecdsa.PublicKey, msg, sig []byte) bool {
	if len(sig) != 64 {
		return ecdsa.VerifyASN1(pk, sha256Sum(msg), sig)
	}
	r, ss := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pk, sha256Sum(msg), r, ss)
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// RotateSession 用页面对挑战的签名换发新令牌。新令牌先处于待确认状态，浏览器带着它来请求以后才转正。
func (s *Service) RotateSession(ctx context.Context, token string, signature []byte, info RequestInfo) (string, error) {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return "", connect.NewError(connect.CodeUnauthenticated, err)
	}
	if st.Challenge == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition, i18n.New("现在不需要更换会话令牌"))
	}
	pk, err := parseP256(st.PublicKey)
	if err != nil || !verifyP1363(pk, []byte(st.Challenge), signature) {
		_ = s.q.UpdateSession(ctx, st.ID, map[string]any{"reverify": ReasonSignature, "challenge": "", "challenge_at": int64(0)})
		s.securityEvent(ctx, st, info, "session_signature_invalid", i18n.Errorf("账号 %s 的登录拿不出会话密钥的签名，已要求重新验证", st.Username).Error(), nil)
		return "", s.reverifyError(ctx, st.UserID, ReasonSignature)
	}
	next, err := RandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if err := s.q.AddPendingToken(ctx, st.ID, HashToken(next), now); err != nil {
		return "", err
	}
	if err := s.q.UpdateSession(ctx, st.ID, map[string]any{"challenge": "", "challenge_at": int64(0), "rotated_at": now}); err != nil {
		return "", err
	}
	return next, nil
}

// liveSession 找令牌对应的会话，不做设备和网络的检查（登记密钥、轮换、重新验证用）。换下来超过宽限期的旧令牌不算。
func (s *Service) liveSession(ctx context.Context, token string) (store.SessionState, string, error) {
	if token == "" {
		return store.SessionState{}, "", i18n.New("请先登录")
	}
	st, state, retiredAt, err := s.q.SessionByToken(ctx, HashToken(token))
	if err != nil {
		return st, "", i18n.New("请先登录")
	}
	if state == store.TokenRetired && time.Since(time.Unix(retiredAt, 0)) > reuseGrace {
		return st, "", errLoggedOut
	}
	if st.ExpiresAt < time.Now().Unix() {
		return st, "", i18n.New("登录已过期，请重新登录")
	}
	return st, state, nil
}

// reverifyError 是要求重新验证的接口错误：Unauthenticated，响应头里写原因和验证方式。
func (s *Service) reverifyError(ctx context.Context, userID int64, reason string) error {
	e := connect.NewError(connect.CodeUnauthenticated, i18n.New("请重新验证身份"))
	e.Meta().Set(ReverifyHeader, reason)
	method := "password"
	if s.TOTPEnabled(ctx, userID) {
		method = "totp"
	}
	e.Meta().Set(ReverifyMethodHeader, method)
	return e
}

// Reverify 重新验证身份：开了两步验证的输验证码，没开的输密码。成功后会话继续；失败计入账号的失败次数，
// 连续 3 次失败退出这个会话。
func (s *Service) Reverify(ctx context.Context, token, password, code string, info RequestInfo) error {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	done, err := s.beginVerify(LimitKey(info.IP), userLimitKey(st.UserID))
	if err != nil {
		return err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	user, err := s.q.GetUserByID(ctx, st.UserID)
	if err != nil {
		result = outcomeNeutral
		return err
	}
	var verr error
	if user.TotpSecret != "" {
		if strings.TrimSpace(code) == "" {
			result = outcomeNeutral
			return connect.NewError(connect.CodeFailedPrecondition, ErrTOTPRequired)
		}
		verr = s.useTOTP(ctx, user, code)
	} else {
		ok, err := verifyLimited(ctx, password, user.PasswordHash)
		if err != nil {
			result = outcomeNeutral
			return err
		}
		if !ok {
			verr = i18n.New("密码不正确")
		}
	}
	at, aid, an := userActor(st.UserID, st.Username)
	if verr != nil {
		failures := st.ReverifyFailures + 1
		s.loginEvent(ctx, audit.Event{Action: "reverify_failed", ActorType: at, ActorID: aid, ActorName: an, IP: info.IP,
			Session: audit.SessionTag(st.ID), Result: audit.ResultFailed, Error: verr.Error()})
		if failures >= reverifyLimit {
			s.endSession(ctx, st, info, "reverify_locked", "", false)
			return connect.NewError(connect.CodeUnauthenticated, i18n.New("验证失败次数过多，已退出登录"))
		}
		_ = s.q.UpdateSession(ctx, st.ID, map[string]any{"reverify_failures": failures})
		return connect.NewError(connect.CodePermissionDenied, verr)
	}
	result = outcomeSuccess
	// 验证通过：当前的设备、网络和指纹都算可信的；拿不出签名的会话可以重新登记会话密钥。
	now := time.Now()
	var n nets
	_ = json.Unmarshal([]byte(st.Nets), &n)
	n.Verified = map[string]string{}
	n.Fam = map[string]*famNets{}
	if fam, network := Network(info.IP); fam != "" {
		n.Fam[fam] = &famNets{Cur: network}
		if s.Policy(ctx).Bind == "ip" {
			n.Verified[fam] = info.IP
		} else {
			n.Verified[fam] = network
		}
	}
	b, _ := json.Marshal(n)
	set := map[string]any{"reverify": "", "reverify_failures": int64(0), "nets": string(b), "rotated_at": now.Unix(),
		"challenge": "", "challenge_at": int64(0), "user_agent": info.UserAgent, "last_ip": info.IP, "last_seen": now.Unix()}
	if info.Fingerprint != "" {
		set["tls_fp"] = info.Fingerprint
	}
	if st.Reverify == ReasonNoKey || st.Reverify == ReasonSignature || len(st.PublicKey) == 0 {
		set["public_key"], set["key_until"] = []byte{}, now.Add(keyWindow).Unix()
	}
	if err := s.q.UpdateSession(ctx, st.ID, set); err != nil {
		return err
	}
	s.loginEvent(ctx, audit.Event{Action: "reverify", ActorType: at, ActorID: aid, ActorName: an, IP: info.IP,
		Session: audit.SessionTag(st.ID), Params: `{"reason":"` + st.Reverify + `"}`})
	return nil
}

// SessionByID 返回会话的当前状态，给不走拦截器的入口（终端、日志流）按会话 ID 重查用。
func (s *Service) SessionByID(ctx context.Context, id string) (User, error) {
	st, err := s.q.SessionByID(ctx, id)
	if err != nil {
		return User{}, i18n.New("请先登录")
	}
	if st.ExpiresAt < time.Now().Unix() {
		return User{}, i18n.New("登录已过期，请重新登录")
	}
	if st.Reverify != "" {
		return User{}, &ErrReverify{Reason: st.Reverify}
	}
	return userOf(st), nil
}
