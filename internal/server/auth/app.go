package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth/attestation"
	"github.com/SIPC/nagi/internal/server/store"
)

// 安卓 App 的会话（见 docs/android.md）：手机在 Keystore 里有三把硬件密钥，用设备密钥（或确认密钥）签名换取 10 分钟的
// 令牌。令牌只在面板进程的内存里（存哈希），绑定换取时的网段。配对码、挑战也只在内存里。

const (
	// AppTokenPrefix 是 App 令牌的前缀。也以 TokenPrefix 开头，但 api_tokens 表里没有它，当访问令牌用会被拒绝。
	AppTokenPrefix = "nagi_app_"
	// AppTokenTTL 是 App 令牌的有效期。
	AppTokenTTL = 10 * time.Minute
	// AppPairingTTL 是配对码的有效期。
	AppPairingTTL = 5 * time.Minute
	// MaxAppDevices 是每个成员最多添加的手机数。
	MaxAppDevices = 10
	// AppPackage 是 App 的包名，证明里的包名要和它一致。
	AppPackage = "ink.sipc.nagi"
	// SettingAppUnlock 是解锁 App 的时长（分钟）在 settings 表里的键。
	SettingAppUnlock = "app_unlock_minutes"
	// DefaultAppUnlockMinutes 是默认的解锁时长。
	DefaultAppUnlockMinutes = 15
	// AppIdleDays 是多久没有换取过令牌的手机自动退出。
	AppIdleDays = 30
	// RetokenHeader 告诉 App 要重新换取令牌（network：换了网段）。
	RetokenHeader = "Nagi-App-Retoken"
	// ElevatedByPassword 是用密码或验证码做的高权限确认（sessions.elevated_by）。
	ElevatedByPassword = "password"

	// appChallengeTTL 是 Challenge 发的挑战的有效期；appNextChallengeTTL 是 Token 一起返回、下一次换取用的。
	// App 在前台时每 4 分钟换一次，令牌 10 分钟过期，所以下一次的挑战要比令牌活得久一点。
	appChallengeTTL     = time.Minute
	appNextChallengeTTL = 15 * time.Minute
	// appSigFailLimit：连续这么多次签名不对，要先解锁 App（用确认密钥）才能继续换取。
	appSigFailLimit = 5
	// 签名内容开头的用途。
	appTokenPurpose = "nagi-app-token"
)

// 密钥的种类（AppTokenRequest.key）。
const (
	AppKeyDevice  = "device"
	AppKeyConfirm = "confirm"
)

// appState 是 App 会话放在内存里的状态。
type appState struct {
	mu sync.Mutex
	// pairings：配对码的哈希 → 配对；byUser：成员 → 他当前有效的配对码的哈希（每个成员同时只有一个）。
	pairings map[string]*appPairing
	byUser   map[int64]string
	// tokens：令牌的哈希 → 令牌。
	tokens map[string]appToken
	// chKey 签无状态的挑战，usedCh 按过期时间（分钟）分桶记着用过的挑战。
	chKey  []byte
	usedCh map[int64]map[string]bool
	// devKeys：nagi server --dev-app-keys，接受软件密钥和模拟器的证明。
	devKeys bool
	// 手机确认（见 approval.go）：approvals 是等确认的请求，approvalTimes 是每个成员最近发起的时间（限制频率），
	// elevated 是手机做过的高权限确认（设备 ID → 到期时间和 IP）。
	approvals     map[string]*approval
	approvalTimes map[string][]time.Time
	elevated      map[string]appElevation
}

type appPairing struct {
	userID    int64
	sessionID string
	expires   time.Time
	used      bool
	device    string
}

type appToken struct {
	deviceID string
	network  string
	expires  time.Time
}

func (a *appState) init() {
	if a.pairings == nil {
		a.pairings = map[string]*appPairing{}
		a.byUser = map[int64]string{}
		a.tokens = map[string]appToken{}
		a.usedCh = map[int64]map[string]bool{}
		a.chKey = make([]byte, 32)
		_, _ = rand.Read(a.chKey)
	}
}

// SetDevAppKeys 打开开发模式：接受软件密钥和模拟器的证明（只能用启动参数 --dev-app-keys 打开）。
func (s *Service) SetDevAppKeys(on bool) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.devKeys = on
}

func (s *Service) devAppKeys() bool {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	return s.app.devKeys
}

// AppUnlockMinutes 是解锁 App 的时长，5 到 60 分钟。
func (s *Service) AppUnlockMinutes(ctx context.Context) int {
	if v, err := s.q.GetSetting(ctx, SettingAppUnlock); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 60 {
			return n
		}
	}
	return DefaultAppUnlockMinutes
}

// ---- 配对码 ----

// CreateAppPairing 给网页会话的成员生成配对码。旧的作废。只能从网页会话调用：App 里不能再添加别的手机。
func (s *Service) CreateAppPairing(ctx context.Context, u User) (string, time.Time, error) {
	if u.SessionID == "" || u.DeviceID != "" || u.TokenID != 0 {
		return "", time.Time{}, connect.NewError(connect.CodePermissionDenied, i18n.New("只能在网页上添加手机"))
	}
	if n, err := s.q.CountUserAppDevices(ctx, u.ID); err != nil {
		return "", time.Time{}, err
	} else if n >= MaxAppDevices {
		return "", time.Time{}, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("每个成员最多添加 %d 台手机，请先退出不用的手机", MaxAppDevices))
	}
	// 配对码 128 位：一次性、10 分钟有效，足够猜不到；比 RandomToken 短一半，二维码的格子少、好扫。
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(AppPairingTTL)
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.init()
	s.prunePairingsLocked()
	if old, ok := s.app.byUser[u.ID]; ok {
		delete(s.app.pairings, old)
	}
	h := HashToken(code)
	s.app.pairings[h] = &appPairing{userID: u.ID, sessionID: u.SessionID, expires: exp}
	s.app.byUser[u.ID] = h
	return code, exp, nil
}

func (s *Service) prunePairingsLocked() {
	now := time.Now()
	for h, p := range s.app.pairings {
		// 用掉的留到过期，网页还要查到「已添加」。
		if now.After(p.expires.Add(time.Minute)) {
			delete(s.app.pairings, h)
			if s.app.byUser[p.userID] == h {
				delete(s.app.byUser, p.userID)
			}
		}
	}
}

// AppPairingStatus 返回成员自己的配对码有没有用掉、添加的手机名称，过期或作废时 expired。
func (s *Service) AppPairingStatus(ctx context.Context, u User, code string) (used bool, device string, expired bool) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.init()
	p, ok := s.app.pairings[HashToken(code)]
	if !ok || p.userID != u.ID {
		return false, "", true
	}
	if p.used {
		return true, p.device, false
	}
	return false, "", time.Now().After(p.expires)
}

var errPairingNotFound = connect.NewError(connect.CodeNotFound, i18n.New("二维码已失效，请在网页上重新生成"))

// pairingFor 找有效的配对码：没过期、没用过、生成它的网页会话还在。
func (s *Service) pairingFor(ctx context.Context, code string) (*appPairing, string, error) {
	h := HashToken(code)
	s.app.mu.Lock()
	s.app.init()
	p, ok := s.app.pairings[h]
	var cp appPairing
	if ok {
		cp = *p
	}
	s.app.mu.Unlock()
	if !ok || cp.used || time.Now().After(cp.expires) {
		return nil, "", errPairingNotFound
	}
	if _, err := s.q.SessionByID(ctx, cp.sessionID); err != nil {
		// 生成它的网页会话退出了：配对码作废。
		s.app.mu.Lock()
		delete(s.app.pairings, h)
		s.app.mu.Unlock()
		return nil, "", errPairingNotFound
	}
	return &cp, h, nil
}

// limitApp 按 IP 限速，和登录共用计数。不认识的配对码、设备 ID 算一次失败。
func (s *Service) limitApp(ip string) (func(outcome), error) {
	done, wait := s.limiter.begin(LimitKey(ip))
	if done == nil && wait == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("上一次请求仍在处理，请稍后重试"))
	}
	if wait > 0 {
		s.rateLimited(ip)
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("失败次数过多，请 %d 秒后重试", int(wait.Seconds())+1))
	}
	return done, nil
}

// PairingUser 是 GetPairing 返回的成员。
type PairingUser struct {
	ID          int64
	Username    string
	DisplayName string
	Role        string
	Avatar      []byte
}

// GetAppPairing 查询配对码属于哪个成员（App 显示「添加到 <面板>，成员 <名称>」）。
func (s *Service) GetAppPairing(ctx context.Context, ip, code string) (PairingUser, error) {
	done, err := s.limitApp(ip)
	if err != nil {
		return PairingUser{}, err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	p, _, err := s.pairingFor(ctx, code)
	if err != nil {
		return PairingUser{}, err
	}
	row, err := s.q.GetUserByID(ctx, p.userID)
	if err != nil {
		result = outcomeNeutral
		return PairingUser{}, err
	}
	result = outcomeNeutral
	perms, err := s.q.RolePerms(ctx, row.RoleID)
	if err != nil {
		return PairingUser{}, err
	}
	var u User
	u.applyRole(row.Role, row.RoleID, perms, row.AllProjects, row.AllNodes, row.NodesFollow)
	return PairingUser{ID: row.ID, Username: row.Username, DisplayName: row.DisplayName, Role: u.Role, Avatar: row.Avatar}, nil
}

// AppKeyProof 是一把密钥的公钥（SPKI）和证明证书链。
type AppKeyProof struct {
	PublicKey   []byte
	Attestation [][]byte
}

// AppDeviceInfo 是手机自己报的信息，只用于显示。
type AppDeviceInfo struct {
	Model, AndroidVersion, AppVersion string
	SDK                               int
}

// PairResult 是添加成功的手机。
type PairResult struct {
	DeviceID string
	Name     string
	UserID   int64
}

// keyRule 是一把密钥要满足的使用条件（见 docs/android.md#密钥）。
type keyRule struct {
	name  string
	check func(r *attestation.Result, sdk int) string
}

var appKeyRules = []keyRule{
	{"device", func(r *attestation.Result, _ int) string {
		if !r.Either(func(l attestation.AuthList) bool { return l.UnlockedDeviceRequired }) {
			return "设备密钥没有要求手机解锁"
		}
		return ""
	}},
	{"confirm", func(r *attestation.Result, sdk int) string {
		if r.HW.NoAuthRequired || r.SW.NoAuthRequired || !r.HW.HasUserAuthType {
			return "确认密钥没有要求验证身份"
		}
		// 每次使用都要验证：没有 authTimeout，或者为 0。
		if r.HW.HasAuthTimeout && r.HW.AuthTimeout != 0 {
			return "确认密钥没有要求每次使用都验证"
		}
		want := int64(attestation.AuthFingerprint)
		if sdk >= 30 {
			want |= attestation.AuthPassword
		}
		if r.HW.UserAuthType&want == 0 {
			return "确认密钥的验证方式不正确"
		}
		return ""
	}},
	{"push", func(r *attestation.Result, _ int) string {
		if !r.HW.NoAuthRequired && !r.SW.NoAuthRequired {
			return "推送密钥的使用条件不正确"
		}
		return ""
	}},
}

// keyCheck 是一把密钥的校验结果。
type keyCheck struct {
	level      string
	bootLocked bool
	patch      int64
	// serials 是证明链里叶子以外各张证书的序列号，没有证明时为空。
	serials []string
	// hardwareRoot：证明链追溯到 Google 的硬件证明根证书（开发模式下可以不是）。
	hardwareRoot bool
}

// checkAppKey 校验一把密钥：公钥是 P-256，证明链、吊销列表、挑战、包名、硬件、使用条件。开发模式下没有证明时当作软件密钥。
func checkAppKey(k AppKeyProof, rule keyRule, challenge []byte, sdk int, dev bool, now time.Time, rev *attestation.Revocations) (keyCheck, error) {
	if _, err := parseP256(k.PublicKey); err != nil {
		return keyCheck{}, errors.New(rule.name + " 公钥格式不正确")
	}
	if len(k.Attestation) == 0 {
		if dev {
			return keyCheck{level: "software"}, nil
		}
		return keyCheck{}, errors.New("没有密钥证明")
	}
	r, err := attestation.Parse(k.Attestation, now)
	if err != nil {
		return keyCheck{}, err
	}
	// 被吊销的证明密钥（泄露的 keybox）能给任意密钥签出有效的证明，开发模式下也不接受。
	if serial, reason, revoked := rev.Check(r.Serials); revoked {
		return keyCheck{}, fmt.Errorf("证明证书 %s 已被 Google 吊销（%s）", serial, reason)
	}
	if !bytes.Equal(r.PublicKey, k.PublicKey) {
		return keyCheck{}, errors.New("证明里的密钥和提交的公钥不一致")
	}
	if !bytes.Equal(r.Challenge, challenge) {
		return keyCheck{}, errors.New("证明的挑战不正确")
	}
	if !slices.Contains(r.Packages, AppPackage) {
		return keyCheck{}, errors.New("证明里的应用不是 Nagi")
	}
	level := "software"
	switch r.SecurityLevel {
	case attestation.TrustedEnvironment:
		level = "tee"
	case attestation.StrongBox:
		level = "strongbox"
	}
	if !dev {
		if !r.HardwareRoot {
			return keyCheck{}, errors.New("证明不是由 Google 的硬件证明根证书签发的")
		}
		if level == "software" {
			return keyCheck{}, errors.New("密钥不在硬件里")
		}
		if !r.HW.HasOrigin || r.HW.Origin != attestation.OriginGenerated {
			return keyCheck{}, errors.New("密钥不是在手机的硬件里生成的")
		}
		if msg := rule.check(r, sdk); msg != "" {
			return keyCheck{}, errors.New(msg)
		}
	}
	return keyCheck{level: level, bootLocked: r.BootLocked(), patch: r.PatchLevel(), serials: r.Serials, hardwareRoot: r.HardwareRoot}, nil
}

// PairApp 用配对码添加手机：校验三把密钥的证明，记下设备。证明的挑战是配对码的 SHA-256。
func (s *Service) PairApp(ctx context.Context, ip, code string, keys [3]AppKeyProof, info AppDeviceInfo) (PairResult, error) {
	done, err := s.limitApp(ip)
	if err != nil {
		return PairResult{}, err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	p, h, err := s.pairingFor(ctx, code)
	if err != nil {
		return PairResult{}, err
	}
	result = outcomeNeutral
	user, err := s.q.GetUserByID(ctx, p.userID)
	if err != nil {
		return PairResult{}, err
	}
	if n, err := s.q.CountUserAppDevices(ctx, user.ID); err != nil {
		return PairResult{}, err
	} else if n >= MaxAppDevices {
		return PairResult{}, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("每个成员最多添加 %d 台手机，请先退出不用的手机", MaxAppDevices))
	}
	info.Model = cleanDeviceText(info.Model, 64)
	info.AndroidVersion = cleanDeviceText(info.AndroidVersion, 16)
	info.AppVersion = cleanDeviceText(info.AppVersion, 32)
	dev := s.devAppKeys()
	challenge := sha256.Sum256([]byte(code))
	now := time.Now()
	rev := s.Revocations()
	var checks [3]keyCheck
	for i, rule := range appKeyRules {
		c, err := checkAppKey(keys[i], rule, challenge[:], info.SDK, dev, now, rev)
		if err != nil {
			msg := i18n.Errorf("密钥证明不通过：%s", err.Error()).Error()
			at, aid, an := userActor(user.ID, user.Username)
			b, _ := json.Marshal(map[string]string{"key": rule.name, "reason": err.Error(), "model": info.Model})
			s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_attestation_failed", ActorType: at, ActorID: aid,
				ActorName: an, IP: ip, Via: audit.ViaApp, Result: audit.ResultDenied, Params: string(b)})
			if s.Alert != nil {
				s.Alert(context.WithoutCancel(ctx), "app-attestation:"+strconv.FormatInt(user.ID, 10), i18n.New("添加手机被拒绝").Error(),
					i18n.Errorf("账号 %s 添加手机 %s 时密钥证明不通过：%s。来源：%s", user.Username, info.Model, err.Error(), ip).Error())
			}
			return PairResult{}, connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
		}
		checks[i] = c
	}
	// 手机的安全级别取三把里最低的，bootloader 和补丁日期以设备密钥的为准。
	level := "strongbox"
	for _, c := range checks {
		if c.level == "software" || c.level == "tee" && level == "strongbox" {
			level = c.level
		}
	}
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	id := base64.RawURLEncoding.EncodeToString(idb)
	name := info.Model
	if name == "" {
		name = "Android"
	}
	// 配对码只能用一次：先占住，再写数据库。
	s.app.mu.Lock()
	cur, ok := s.app.pairings[h]
	if !ok || cur.used {
		s.app.mu.Unlock()
		return PairResult{}, errPairingNotFound
	}
	cur.used, cur.device = true, name
	s.app.mu.Unlock()
	d := store.AppDevice{ID: id, UserID: user.ID, Name: name, Model: info.Model, AndroidVersion: info.AndroidVersion, AppVersion: info.AppVersion,
		DeviceKey: keys[0].PublicKey, ConfirmKey: keys[1].PublicKey, PushKey: keys[2].PublicKey, SecurityLevel: level,
		BootLocked: checks[0].bootLocked || level == "software", PatchLevel: checks[0].patch, DevKeys: dev,
		CreatedAt: now.Unix(), LastUsedAt: now.Unix(), LastIP: ip, LastUnlockAt: now.Unix()}
	if level == "software" {
		d.BootLocked = false
	}
	// 三把密钥都有追溯到 Google 根证书的证明时才算检查过吊销（开发模式下自己的根证书不算）；
	// 记下各条链的序列号（去重），拿到新的吊销列表时复查。
	d.RevocationChecked = true
	seen := map[string]bool{}
	var serials []string
	for _, c := range checks {
		if len(c.serials) == 0 || !c.hardwareRoot {
			d.RevocationChecked = false
		}
		for _, sn := range c.serials {
			if !seen[sn] {
				seen[sn] = true
				serials = append(serials, sn)
			}
		}
	}
	d.AttestationSerials = strings.Join(serials, " ")
	if err := s.q.CreateAppDevice(ctx, d); err != nil {
		s.app.mu.Lock()
		cur.used, cur.device = false, ""
		s.app.mu.Unlock()
		return PairResult{}, err
	}
	// 检查以后、写入之前换了列表时，这台手机可能不在那次复查里：让 RevocationLoop 按新的列表再复查一遍。
	if s.Revocations() != rev {
		s.requestRecheck()
	}
	at, aid, an := userActor(user.ID, user.Username)
	b, _ := json.Marshal(map[string]any{"device": name, "android": info.AndroidVersion, "app": info.AppVersion, "level": level,
		"boot_locked": d.BootLocked, "dev_keys": dev})
	s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_paired", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
		Via: audit.ViaApp, Client: name, Session: audit.SessionTag("app:" + id), Params: string(b)})
	if s.Alert != nil {
		msg := i18n.Errorf("账号：%s\n手机：%s（Android %s）\n来源：%s", user.Username, name, info.AndroidVersion, ip).Error()
		if !d.BootLocked {
			msg += "\n" + i18n.New("这台手机的 bootloader 没有锁定").Error()
		}
		if dev {
			msg += "\n" + i18n.New("面板运行在开发模式（--dev-app-keys），接受软件密钥").Error()
		}
		s.Alert(context.WithoutCancel(ctx), "app-paired:"+id, i18n.Errorf("账号 %s 添加了手机 %s", user.Username, name).Error(), msg)
	}
	return PairResult{DeviceID: id, Name: name, UserID: user.ID}, nil
}

// cleanDeviceText 去掉控制字符，限制长度：这些是手机自己报的，只用于显示。
func cleanDeviceText(v string, max int) string {
	v = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v))
	if utf8.RuneCountInString(v) > max {
		v = string([]rune(v)[:max])
	}
	return v
}

// ---- 挑战和令牌 ----

// newAppChallenge 生成无状态的挑战：随机数、过期时间、设备 ID 用内存里的密钥做 HMAC。面板重启后作废。
func (s *Service) newAppChallenge(deviceID string, ttl time.Duration) string {
	s.app.mu.Lock()
	s.app.init()
	key := s.app.chKey
	s.app.mu.Unlock()
	b := make([]byte, 24)
	_, _ = rand.Read(b[:16])
	binary.BigEndian.PutUint64(b[16:], uint64(time.Now().Add(ttl).Unix()))
	m := hmac.New(sha256.New, key)
	m.Write(b)
	m.Write([]byte(deviceID))
	return base64.RawURLEncoding.EncodeToString(append(b, m.Sum(nil)...))
}

// useAppChallenge 核对挑战属于这台手机、没过期、没用过，并记为用过。
func (s *Service) useAppChallenge(deviceID, challenge string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(raw) != 24+sha256.Size {
		return false
	}
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.init()
	m := hmac.New(sha256.New, s.app.chKey)
	m.Write(raw[:24])
	m.Write([]byte(deviceID))
	if !hmac.Equal(m.Sum(nil), raw[24:]) {
		return false
	}
	exp := int64(binary.BigEndian.Uint64(raw[16:24]))
	now := time.Now().Unix()
	if exp < now {
		return false
	}
	bucket := exp / 60
	for b := range s.app.usedCh {
		if b < now/60 {
			delete(s.app.usedCh, b)
		}
	}
	if s.app.usedCh[bucket] == nil {
		s.app.usedCh[bucket] = map[string]bool{}
	}
	if s.app.usedCh[bucket][challenge] {
		return false
	}
	s.app.usedCh[bucket][challenge] = true
	return true
}

var errDeviceNotFound = connect.NewError(connect.CodeNotFound, i18n.New("这台手机已退出，请重新添加"))

// AppChallenge 返回换取令牌用的挑战。设备 ID 不认识时 not_found。
func (s *Service) AppChallenge(ctx context.Context, ip, deviceID string) (string, error) {
	done, err := s.limitApp(ip)
	if err != nil {
		return "", err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	if _, err := s.q.AppDeviceByID(ctx, deviceID); errors.Is(err, sql.ErrNoRows) {
		return "", errDeviceNotFound
	} else if err != nil {
		result = outcomeNeutral
		return "", err
	}
	result = outcomeNeutral
	return s.newAppChallenge(deviceID, appChallengeTTL), nil
}

// AppTokenSigned 是签名的内容：用途、连接的主机、设备 ID、挑战，换行分隔。
func AppTokenSigned(host, deviceID, challenge string) []byte {
	return []byte(appTokenPurpose + "\n" + host + "\n" + deviceID + "\n" + challenge)
}

// AppTokenRequest 是一次换取。
type AppTokenRequest struct {
	DeviceID, Challenge, Key, Host string
	Signature                      []byte
}

// AppTokenResult 是换取的结果。
type AppTokenResult struct {
	Token         string
	Expires       time.Time
	NextChallenge string
	UnlockMinutes int
}

// AppToken 用设备密钥或确认密钥的签名换取令牌。hosts 是面板自己的地址和域名，签名里的主机要是其中之一。
// standby：备用面板不写数据库（不更新最近活动和解锁时间）。
func (s *Service) AppToken(ctx context.Context, ip string, req AppTokenRequest, hosts []string, standby bool) (AppTokenResult, error) {
	done, err := s.limitApp(ip)
	if err != nil {
		return AppTokenResult{}, err
	}
	result := outcomeFailure
	defer func() { done(result) }()
	d, err := s.q.AppDeviceByID(ctx, req.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return AppTokenResult{}, errDeviceNotFound
	} else if err != nil {
		result = outcomeNeutral
		return AppTokenResult{}, err
	}
	if !s.useAppChallenge(req.DeviceID, req.Challenge) {
		result = outcomeNeutral
		return AppTokenResult{}, connect.NewError(connect.CodeFailedPrecondition, i18n.New("挑战已过期，请重试"))
	}
	if !slices.Contains(hosts, req.Host) && !s.devAppKeys() {
		return AppTokenResult{}, connect.NewError(connect.CodePermissionDenied, i18n.New("连接的地址不是这个面板"))
	}
	at, aid, an := userActor(d.UserID, d.Username)
	tag := audit.SessionTag("app:" + d.ID)
	if d.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.adminNetworkDenied(ctx, d.UserID, d.Username, ip, "app")
		return AppTokenResult{}, errAdminNetwork()
	}
	spki := d.DeviceKey
	if req.Key == AppKeyConfirm {
		spki = d.ConfirmKey
	} else if req.Key != AppKeyDevice {
		return AppTokenResult{}, connect.NewError(connect.CodeInvalidArgument, i18n.New("密钥种类不正确"))
	}
	pk, err := parseP256(spki)
	now := time.Now()
	if err != nil || !ecdsa.VerifyASN1(pk, sha256Sum(AppTokenSigned(req.Host, d.ID, req.Challenge)), req.Signature) {
		n := d.SigFailures + 1
		if !standby {
			_ = s.q.SetAppDeviceSigFailures(ctx, d.ID, n, n >= appSigFailLimit)
		}
		b, _ := json.Marshal(map[string]any{"key": req.Key, "failures": n})
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_signature_invalid", ActorType: at, ActorID: aid, ActorName: an,
			IP: ip, Via: audit.ViaApp, Client: d.Name, Session: tag, Result: audit.ResultDenied, Params: string(b)})
		return AppTokenResult{}, connect.NewError(connect.CodeUnauthenticated, i18n.New("签名不正确"))
	}
	unlock := s.AppUnlockMinutes(ctx)
	if req.Key == AppKeyDevice && (d.SigFailures >= appSigFailLimit || now.Sub(time.Unix(d.LastUnlockAt, 0)) > time.Duration(unlock)*time.Minute) {
		result = outcomeNeutral
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_unlock_required", ActorType: at, ActorID: aid, ActorName: an,
			IP: ip, Via: audit.ViaApp, Client: d.Name, Session: tag, Result: audit.ResultDenied})
		e := connect.NewError(connect.CodeFailedPrecondition, i18n.New("请先解锁 App"))
		e.Meta().Set(RetokenHeader, "unlock")
		return AppTokenResult{}, e
	}
	result = outcomeNeutral
	if !standby {
		// 每次成功换取都把解锁时间往后延：连续使用时不会反复要求验证。
		if err := s.q.TouchAppDevice(ctx, d.ID, now.Unix(), ip, now.Unix()); err != nil {
			return AppTokenResult{}, err
		}
	}
	token := AppTokenPrefix + must(RandomToken())
	_, network := Network(ip)
	exp := now.Add(AppTokenTTL)
	s.app.mu.Lock()
	s.app.init()
	for h, t := range s.app.tokens {
		// 过期一小时以后才丢：安全入口据此放行，接口再让 App 重新换取。
		if now.After(t.expires.Add(time.Hour)) {
			delete(s.app.tokens, h)
		}
	}
	s.app.tokens[HashToken(token)] = appToken{deviceID: d.ID, network: network, expires: exp}
	s.app.mu.Unlock()
	if req.Key == AppKeyConfirm {
		s.loginEvent(ctx, audit.Event{Action: "app_unlocked", ActorType: at, ActorID: aid, ActorName: an, IP: ip, Via: audit.ViaApp,
			Client: d.Name, Session: tag})
	}
	return AppTokenResult{Token: token, Expires: exp, NextChallenge: s.newAppChallenge(d.ID, appNextChallengeTTL), UnlockMinutes: unlock}, nil
}

func must(v string, err error) string {
	if err != nil {
		panic(err)
	}
	return v
}

// ErrAppRetoken 表示令牌要重新换取（过期、网段变了、面板重启过），App 自动处理，用户看不到。
type ErrAppRetoken struct{ Reason string }

func (e *ErrAppRetoken) Error() string { return i18n.New("请重新换取令牌").Error() }

// AppBearer 从 Authorization 请求头取出 App 令牌。
func AppBearer(h http.Header) string {
	v := h.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") && strings.HasPrefix(v[7:], AppTokenPrefix) {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// AppTokenUser 核对 App 令牌，返回手机所属的成员（角色和访问范围按成员当前的设置）。
func (s *Service) AppTokenUser(ctx context.Context, token, ip string) (User, error) {
	h := HashToken(token)
	s.app.mu.Lock()
	s.app.init()
	t, ok := s.app.tokens[h]
	if ok && time.Now().After(t.expires) {
		ok = false
	}
	s.app.mu.Unlock()
	if !ok {
		return User{}, &ErrAppRetoken{Reason: "expired"}
	}
	if _, network := Network(ip); network != t.network {
		return User{}, &ErrAppRetoken{Reason: "network"}
	}
	d, err := s.q.AppDeviceByID(ctx, t.deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		s.dropDeviceTokens(t.deviceID)
		return User{}, errDeviceNotFound
	} else if err != nil {
		return User{}, err
	}
	if d.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		return User{}, i18n.New("管理员只能从允许的地址登录")
	}
	u := User{ID: d.UserID, Name: d.Username, Via: audit.ViaApp, DeviceID: d.ID, DeviceName: d.Name}
	// 这台手机做过的高权限确认（AppElevate），只对确认时的 IP 有效。
	if until := s.appElevatedUntil(d.ID, ip); until > 0 {
		u.ElevatedUntil, u.ElevatedBy = until, ElevatedByApp
	}
	u.applyRole(d.Role, d.RoleID, d.Perms, d.AllProjects, d.AllNodes, d.NodesFollow)
	return u, nil
}

// knownAppToken 判断令牌是面板发过的（不管有没有过期）。
func (s *Service) knownAppToken(token string) bool {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	_, ok := s.app.tokens[HashToken(token)]
	return ok
}

// dropDeviceTokens 让这台手机在内存里的令牌立即作废。
func (s *Service) dropDeviceTokens(deviceID string) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	for h, t := range s.app.tokens {
		if t.deviceID == deviceID {
			delete(s.app.tokens, h)
		}
	}
	delete(s.app.elevated, deviceID)
}

// RemoveAppDevice 退出成员的一台手机：删除设备记录，令牌立即作废。不是这个成员的手机时返回 false。
func (s *Service) RemoveAppDevice(ctx context.Context, userID int64, username, deviceID, reason string) (bool, error) {
	n, err := s.q.DeleteAppDevice(ctx, userID, deviceID)
	if err != nil || n == 0 {
		return false, err
	}
	s.dropDeviceTokens(deviceID)
	at, aid, an := userActor(userID, username)
	s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_removed", ActorType: at, ActorID: aid, ActorName: an,
		Via: audit.ViaApp, Session: audit.SessionTag("app:" + deviceID), Params: `{"reason":"` + reason + `"}`})
	return true, nil
}

// RemoveUserAppDevices 退出成员的全部手机（改密码、重置密码）。
func (s *Service) RemoveUserAppDevices(ctx context.Context, userID int64) error {
	list, err := s.q.UserAppDevices(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.q.DeleteUserAppDevices(ctx, userID); err != nil {
		return err
	}
	for _, d := range list {
		s.dropDeviceTokens(d.ID)
	}
	return nil
}

// cleanupAppDevices 退出 30 天没有换取过令牌的手机。
func (s *Service) cleanupAppDevices(ctx context.Context) {
	list, err := s.q.DeleteIdleAppDevices(ctx, time.Now().Add(-AppIdleDays*24*time.Hour).Unix())
	if err != nil {
		return
	}
	for _, d := range list {
		s.dropDeviceTokens(d.ID)
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_removed", ActorType: audit.ActorUser,
			ActorID: strconv.FormatInt(d.UserID, 10), Via: audit.ViaApp, Session: audit.SessionTag("app:" + d.ID), Params: `{"reason":"idle"}`})
	}
}

// appSPKI 把公钥编码成 SPKI（测试用）。
func appSPKI(pk *ecdsa.PublicKey) []byte {
	b, _ := x509.MarshalPKIXPublicKey(pk)
	return b
}
