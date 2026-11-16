package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

// 通行密钥（Passkey，见 docs/panel.md#通行密钥）：绑定域名以后可以添加，用来代替密码和验证码登录。
// 通行密钥只对添加时的域名有效，浏览器不会在别的域名下使用它，钓鱼代理转发登录页也拿不到可用的签名。

// settingDomain 是面板绑定的域名（和 api.SettingDomain 相同）。
const settingDomain = "panel_domain"

const (
	passkeyChallengeTTL = 5 * time.Minute
	// maxPasskeyChallenges：没登录的人也能要挑战，限制同时有效的个数。
	maxPasskeyChallenges = 1000
	maxPasskeys          = 20
)

type passkeyChallenge struct {
	challenge []byte
	userID    int64 // 添加时是成员，登录时为 0
	rpID      string
	expires   time.Time
}

// PasskeyDomain 返回能用通行密钥的域名（面板绑定的域名），没绑定时为空。
func (s *Service) PasskeyDomain(ctx context.Context) string {
	v, _ := s.q.GetSetting(ctx, settingDomain)
	return strings.ToLower(strings.TrimSpace(v))
}

// newPasskeyChallenge 生成一次性的挑战，返回编号和内容。
func (s *Service) newPasskeyChallenge(userID int64, rpID string) (string, []byte, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	id, challenge := hex.EncodeToString(b[:16]), b[16:]
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenges == nil {
		s.challenges = map[string]passkeyChallenge{}
	}
	for k, c := range s.challenges {
		if now.After(c.expires) {
			delete(s.challenges, k)
		}
	}
	if len(s.challenges) >= maxPasskeyChallenges {
		return "", nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("请求过多，请稍后重试"))
	}
	s.challenges[id] = passkeyChallenge{challenge: challenge, userID: userID, rpID: rpID, expires: now.Add(passkeyChallengeTTL)}
	return id, challenge, nil
}

// takePasskeyChallenge 取出挑战，只能用一次。
func (s *Service) takePasskeyChallenge(id string, userID int64) (passkeyChallenge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[id]
	delete(s.challenges, id)
	if !ok || time.Now().After(c.expires) || c.userID != userID {
		return passkeyChallenge{}, false
	}
	return c, true
}

func errNoDomain() error {
	return connect.NewError(connect.CodeFailedPrecondition, i18n.New("绑定域名后才能使用通行密钥"))
}

// UserHandle 是写进通行密钥的成员标识：成员 ID，8 字节大端。
func UserHandle(userID int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(userID)) }

// PasskeyRegistration 是添加通行密钥时交给浏览器的参数。
type PasskeyRegistration struct {
	ChallengeID string
	Challenge   []byte
	RPID        string
	UserHandle  []byte
	Exclude     [][]byte // 已经添加过的，浏览器不会在同一个验证器上重复添加
}

// BeginPasskeyRegistration 开始添加通行密钥。
func (s *Service) BeginPasskeyRegistration(ctx context.Context, userID int64) (PasskeyRegistration, error) {
	rpID := s.PasskeyDomain(ctx)
	if rpID == "" {
		return PasskeyRegistration{}, errNoDomain()
	}
	existing, err := s.q.UserPasskeys(ctx, userID)
	if err != nil {
		return PasskeyRegistration{}, err
	}
	if len(existing) >= maxPasskeys {
		return PasskeyRegistration{}, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("最多添加 %d 个通行密钥", maxPasskeys))
	}
	id, challenge, err := s.newPasskeyChallenge(userID, rpID)
	if err != nil {
		return PasskeyRegistration{}, err
	}
	r := PasskeyRegistration{ChallengeID: id, Challenge: challenge, RPID: rpID, UserHandle: UserHandle(userID)}
	for _, p := range existing {
		r.Exclude = append(r.Exclude, p.CredentialID)
	}
	return r, nil
}

// AddPasskey 校验浏览器返回的新凭据并保存。
func (s *Service) AddPasskey(ctx context.Context, userID int64, challengeID, name string, clientDataJSON, attestationObject []byte) error {
	c, ok := s.takePasskeyChallenge(challengeID, userID)
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("请求已过期，请重新添加"))
	}
	if s.PasskeyDomain(ctx) != c.rpID {
		return errNoDomain()
	}
	if checkClientData(clientDataJSON, "webauthn.create", c.challenge, c.rpID) != nil {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("通行密钥验证失败，请重新添加"))
	}
	a, err := parseAttestation(attestationObject, c.rpID)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("通行密钥验证失败，验证器不受支持"))
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "通行密钥"
	}
	if len([]rune(name)) > 50 {
		name = string([]rune(name)[:50])
	}
	_, err = s.q.AddPasskey(ctx, store.Passkey{UserID: userID, CredentialID: a.credentialID, PublicKey: a.publicKey,
		SignCount: int64(a.signCount), Name: name, RPID: c.rpID, CreatedAt: time.Now().Unix()})
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return connect.NewError(connect.CodeAlreadyExists, i18n.New("这个通行密钥已经添加过"))
	}
	return err
}

// ListPasskeys 列出成员的通行密钥。
func (s *Service) ListPasskeys(ctx context.Context, userID int64) ([]store.Passkey, error) {
	return s.q.UserPasskeys(ctx, userID)
}

// DeletePasskey 删除成员自己的通行密钥，返回名称。
func (s *Service) DeletePasskey(ctx context.Context, userID, id int64) (string, error) {
	name, err := s.q.DeletePasskey(ctx, userID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", connect.NewError(connect.CodeNotFound, i18n.New("通行密钥不存在"))
	}
	return name, err
}

// BeginPasskeyLogin 开始用通行密钥登录：不指定成员，浏览器列出这个域名下的通行密钥让用户选。
func (s *Service) BeginPasskeyLogin(ctx context.Context) (id string, challenge []byte, rpID string, err error) {
	rpID = s.PasskeyDomain(ctx)
	if rpID == "" {
		return "", nil, "", errNoDomain()
	}
	id, challenge, err = s.newPasskeyChallenge(0, rpID)
	return id, challenge, rpID, err
}

// PasskeyAssertion 是浏览器返回的登录签名。
type PasskeyAssertion struct {
	ChallengeID       string
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
	UserHandle        []byte
}

// errPasskeyLogin 不区分原因：不告诉对方是凭据不存在还是签名不对。
func errPasskeyLogin() error {
	return connect.NewError(connect.CodeUnauthenticated, i18n.New("通行密钥验证失败，请重试或使用密码登录"))
}

// PasskeyLogin 校验签名并登录。通行密钥本身要求验证器验证了用户（指纹、面容、PIN），所以不再要密码和验证码。
// 失败和密码登录一样按 IP 计入限速。
func (s *Service) PasskeyLogin(ctx context.Context, ip string, a PasskeyAssertion) (*http.Cookie, error) {
	done, wait := s.limiter.begin(LimitKey(ip))
	if done == nil && wait == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("上一次登录仍在处理，请稍后重试"))
	}
	if wait > 0 {
		s.rateLimited(ip)
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("登录失败次数过多，请 %d 秒后重试", int(wait.Seconds())+1))
	}
	result := outcomeFailure
	defer func() { done(result) }()
	c, ok := s.takePasskeyChallenge(a.ChallengeID, 0)
	if !ok {
		result = outcomeNeutral
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("请求已过期，请重试"))
	}
	fail := func(username, reason string) error {
		s.loginEvent(ctx, audit.Event{Action: "passkey_failed", ActorType: audit.ActorUser, ActorName: username, IP: ip,
			Result: audit.ResultFailed, Error: reason})
		return errPasskeyLogin()
	}
	p, err := s.q.PasskeyByCredential(ctx, a.CredentialID)
	if errors.Is(err, sql.ErrNoRows) {
		// 不认识的凭据谁都能提交，和猜密码一样按 IP 每小时汇总，不逐条记。
		s.recordLoginFailure(ctx, ip, "")
		return nil, errPasskeyLogin()
	}
	if err != nil {
		result = outcomeNeutral
		return nil, err
	}
	user, err := s.q.GetUserByID(ctx, p.UserID)
	if err != nil {
		result = outcomeNeutral
		return nil, err
	}
	if p.RPID != c.rpID || s.PasskeyDomain(ctx) != c.rpID {
		return nil, fail(user.Username, "通行密钥属于以前绑定的域名")
	}
	if len(a.UserHandle) > 0 && string(a.UserHandle) != string(UserHandle(p.UserID)) {
		return nil, fail(user.Username, "成员标识不符")
	}
	if checkClientData(a.ClientDataJSON, "webauthn.get", c.challenge, c.rpID) != nil {
		return nil, fail(user.Username, "来源或挑战不符")
	}
	ad, err := parseAuthData(a.AuthenticatorData, c.rpID, false)
	if err != nil {
		return nil, fail(user.Username, "验证器数据不符")
	}
	if verifyAssertion(p.PublicKey, a.AuthenticatorData, a.ClientDataJSON, a.Signature) != nil {
		return nil, fail(user.Username, "签名不正确")
	}
	// 签名计数没有变大：通行密钥可能被复制了。
	if ok, err := s.q.UsePasskey(ctx, p.ID, int64(ad.signCount), time.Now().Unix()); err != nil {
		result = outcomeNeutral
		return nil, err
	} else if !ok {
		return nil, fail(user.Username, "签名计数没有增加，通行密钥可能被复制")
	}
	if user.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.adminNetworkDenied(ctx, user.ID, user.Username, ip, "passkey")
		return nil, errAdminNetwork()
	}
	result = outcomeSuccess
	cookie, err := s.NewSession(ctx, user.ID, ip)
	if err == nil {
		at, aid, an := userActor(user.ID, user.Username)
		st, _, _, _ := s.q.SessionByToken(ctx, HashToken(cookie.Value))
		params, _ := json.Marshal(map[string]string{"method": "passkey", "passkey": p.Name})
		s.loginEvent(ctx, audit.Event{Action: "Login", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
			Session: audit.SessionTag(st.ID), Params: string(params)})
		s.checkNewNetwork(ctx, user.ID, user.Username, user.Role, ip, RequestMeta(ctx).UserAgent)
	}
	return cookie, err
}
