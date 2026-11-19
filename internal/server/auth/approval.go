package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
)

// 手机确认（见 docs/android.md#手机确认）：网页发起一个请求，成员在手机 App 上用确认密钥签名（每次使用都要验证指纹、
// 面容或锁屏密码，由硬件保证）确认。用在两处：网页的高权限确认，和登录的第二步（代替验证码）。
// 请求只在面板进程的内存里，2 分钟内有效，面板重启后作废。
//
// 防止随手点了别人发起的请求（知道密码的人反复发起，等成员误点）：网页显示两位数字，成员要在手机上照着输入，
// 数字签在确认里；输错 3 次这个请求作废。每个成员每种请求同时只有一个，10 分钟内最多发起 10 个。

const (
	// ElevatedByApp 是用手机的确认密钥做的高权限确认（sessions.elevated_by）。给自己留登录凭据的操作不认（passwordElevation）。
	ElevatedByApp = "app"

	ApprovalElevate = "elevate"
	ApprovalLogin   = "login"

	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"

	approvalTTL = 2 * time.Minute
	// 确认以后网页多久内要来取结果。
	approvalClaimTTL  = time.Minute
	approvalWindow    = 10 * time.Minute
	approvalPerWindow = 10
	approvalCodeTries = 3
	// 每个成员同时等确认的请求最多这么多个（不同来源各一个）。
	approvalMaxPending = 4

	approvalPurpose   = "nagi-app-approve"
	appElevatePurpose = "nagi-app-elevate"
)

type approval struct {
	id        string
	userID    int64
	username  string
	kind      string
	sessionID string // 高权限确认：发起的会话
	ticket    string // 登录：发起的浏览器手里的凭据的哈希
	ip, agent string
	code      string
	created   time.Time
	expires   time.Time
	status    string
	tries     int
	device    string // 确认的手机名称
}

// appElevation 是一台手机做过的高权限确认。
type appElevation struct {
	until time.Time
	ip    string
}

// Approval 是给网页的一个请求。
type Approval struct {
	ID, Code, Ticket string
	Expires          time.Time
}

// PendingApproval 是给手机看的一个等确认的请求（没有数字）。
type PendingApproval struct {
	ID, Kind, IP, Agent string
	Created, Expires    time.Time
}

var (
	errNoPhone          = connect.NewError(connect.CodeFailedPrecondition, i18n.New("这个账号没有添加手机 App"))
	errApprovalNotFound = connect.NewError(connect.CodeNotFound, i18n.New("请求已失效，请重新发起"))
)

// newApproval 登记一个请求，同一个成员同一种的旧请求作废。调用时不持有 s.app.mu。
func (s *Service) newApproval(a *approval) (*approval, error) {
	now := time.Now()
	a.id = must(RandomToken())
	var n [1]byte
	_, _ = rand.Read(n[:])
	// 0–255 取模 100 略有偏差，这个数字不是秘密（只防误点），不要紧。
	a.code = fmt.Sprintf("%02d", int(n[0])%100)
	a.created, a.expires, a.status = now, now.Add(approvalTTL), ApprovalPending
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	if s.app.approvals == nil {
		s.app.approvals = map[string]*approval{}
		s.app.approvalTimes = map[string][]time.Time{}
	}
	// 限额按「成员、种类、来源」分开算：别处的人（知道密码）发起再多，占不掉成员自己这边的额度，
	// 登录和高权限确认也互不影响。被限额拒绝时不动已有的请求。
	key := strconv.FormatInt(a.userID, 10) + "|" + a.kind + "|" + LimitKey(a.ip)
	times := slices.DeleteFunc(s.app.approvalTimes[key], func(t time.Time) bool { return now.Sub(t) > approvalWindow })
	if len(times) >= approvalPerWindow {
		s.app.approvalTimes[key] = times
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("发起得太频繁，请稍后重试"))
	}
	// 同一个成员、同一种、同一个来源的旧请求作废；别的来源发起的留着，手机上一起列出来，由成员按来源和数字认
	// （否则别处的人能把成员自己正在等的请求顶掉）。每个成员同时最多 approvalMaxPending 个，满了就拒绝新的：
	// 不丢已有的，成员已经在等、或者手机上已经确认还没取走的请求不会被别处的顶掉（2026-11-19-402545d#3）。
	others := 0
	var same []string
	for id, old := range s.app.approvals {
		switch {
		case now.After(old.expires):
			delete(s.app.approvals, id)
		case old.userID == a.userID && old.kind == a.kind && LimitKey(old.ip) == LimitKey(a.ip):
			same = append(same, id)
		case old.userID == a.userID && old.kind == a.kind && (old.status == ApprovalPending || old.status == ApprovalApproved):
			// 只算同一种、还在等确认或者确认了还没取走的：拒绝掉的、输错作废的不占名额，
			// 登录的请求再多也不挡高权限确认（2026-11-19-7b50402#4）。
			others++
		}
	}
	if others >= approvalMaxPending {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.New("等待手机确认的请求太多，请稍后重试或改用验证码"))
	}
	for _, id := range same {
		delete(s.app.approvals, id)
	}
	for k, ts := range s.app.approvalTimes {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > approvalWindow {
			delete(s.app.approvalTimes, k)
		}
	}
	s.app.approvalTimes[key] = append(times, now)
	s.app.approvals[a.id] = a
	return a, nil
}

func (s *Service) hasPhone(ctx context.Context, userID int64) (bool, error) {
	n, err := s.q.CountUserAppDevices(ctx, userID)
	return n > 0, err
}

// HasPhone 判断成员添加过手机 App。
func (s *Service) HasPhone(ctx context.Context, userID int64) bool {
	ok, _ := s.hasPhone(ctx, userID)
	return ok
}

// BeginElevateApproval 发起高权限确认的手机确认，记在这个会话上。
func (s *Service) BeginElevateApproval(ctx context.Context, ip, token string) (Approval, error) {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return Approval{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if ok, err := s.hasPhone(ctx, st.UserID); err != nil {
		return Approval{}, err
	} else if !ok {
		return Approval{}, errNoPhone
	}
	a, err := s.newApproval(&approval{userID: st.UserID, username: st.Username, kind: ApprovalElevate, sessionID: st.ID, ip: ip,
		agent: ParseAgent(RequestMeta(ctx).UserAgent).String()})
	if err != nil {
		return Approval{}, err
	}
	return Approval{ID: a.id, Code: a.code, Expires: a.expires}, nil
}

// ElevateApprovalStatus 是网页轮询的结果。手机确认以后，第一次查到时把这个会话记为做过高权限确认（ElevatedByApp）：
// 只认发起的那个会话，IP 要和发起时一样。
func (s *Service) ElevateApprovalStatus(ctx context.Context, ip, token, id string) (string, time.Time, error) {
	st, _, err := s.liveSession(ctx, token)
	if err != nil {
		return "", time.Time{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	now := time.Now()
	s.app.mu.Lock()
	a := s.app.approvals[id]
	if a == nil || a.kind != ApprovalElevate || a.sessionID != st.ID {
		s.app.mu.Unlock()
		return "", time.Time{}, errApprovalNotFound
	}
	status := a.status
	if now.After(a.expires) {
		status = ApprovalExpired
	}
	if status != ApprovalPending {
		delete(s.app.approvals, id)
	}
	device, from := a.device, a.ip
	s.app.mu.Unlock()
	if status != ApprovalApproved {
		return status, time.Time{}, nil
	}
	if from != ip {
		return ApprovalExpired, time.Time{}, nil
	}
	until := now.Add(ElevationTTL)
	if err := s.q.UpdateSession(ctx, st.ID, map[string]any{"elevated_until": until.Unix(), "elevated_ip": ip, "elevated_by": ElevatedByApp}); err != nil {
		return "", time.Time{}, err
	}
	at, aid, an := userActor(st.UserID, st.Username)
	b, _ := json.Marshal(map[string]string{"device": device})
	s.loginEvent(ctx, audit.Event{Action: "elevated_by_phone", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
		Session: audit.SessionTag(st.ID), Params: string(b)})
	return ApprovalApproved, until, nil
}

// BeginLoginApproval 是登录的第二步改用手机确认：先和登录一样核对密码（限速、计失败），账号要开了两步验证、添加过手机。
// 返回的 Ticket 只给发起的浏览器，凭它取结果。
func (s *Service) BeginLoginApproval(ctx context.Context, ip, username, password string) (Approval, error) {
	done, wait := s.limiter.begin(LimitKey(ip))
	if done == nil && wait == 0 {
		return Approval{}, connect.NewError(connect.CodeResourceExhausted, i18n.New("上一次登录仍在处理，请稍后重试"))
	}
	if wait > 0 {
		s.rateLimited(ip)
		return Approval{}, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("登录失败次数过多，请 %d 秒后重试", int(wait.Seconds())+1))
	}
	result := outcomeFailure
	defer func() { done(result) }()
	user, err := s.q.GetUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		result = outcomeNeutral
		return Approval{}, err
	}
	hash := user.PasswordHash
	if err != nil {
		hash = dummyHash()
	}
	ok, verr := verifyLimited(ctx, password, hash)
	if verr != nil {
		result = outcomeNeutral
		return Approval{}, verr
	}
	if err != nil || !ok {
		s.recordLoginFailure(ctx, ip, username)
		return Approval{}, connect.NewError(connect.CodeUnauthenticated, ErrBadCredentials)
	}
	// 密码对了：和登录时还没填验证码一样，不算失败，也不清零。
	result = outcomeNeutral
	if user.TotpSecret == "" {
		return Approval{}, connect.NewError(connect.CodeFailedPrecondition, i18n.New("这个账号没有开启两步验证，直接登录即可"))
	}
	if user.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.adminNetworkDenied(ctx, user.ID, user.Username, ip, "password")
		return Approval{}, errAdminNetwork()
	}
	if ok, err := s.hasPhone(ctx, user.ID); err != nil {
		return Approval{}, err
	} else if !ok {
		return Approval{}, errNoPhone
	}
	ticket := must(RandomToken())
	a, err := s.newApproval(&approval{userID: user.ID, username: user.Username, kind: ApprovalLogin, ticket: HashToken(ticket), ip: ip,
		agent: ParseAgent(RequestMeta(ctx).UserAgent).String()})
	if err != nil {
		return Approval{}, err
	}
	return Approval{ID: a.id, Code: a.code, Ticket: ticket, Expires: a.expires}, nil
}

// CompleteLoginApproval 是登录页轮询的结果：手机确认以后返回会话 cookie（只给一次）。IP 要和发起时一样。
func (s *Service) CompleteLoginApproval(ctx context.Context, ip, ticket string) (string, *http.Cookie, error) {
	h := HashToken(ticket)
	now := time.Now()
	s.app.mu.Lock()
	var a *approval
	for _, x := range s.app.approvals {
		if x.kind == ApprovalLogin && subtle.ConstantTimeCompare([]byte(x.ticket), []byte(h)) == 1 {
			a = x
			break
		}
	}
	if a == nil || a.ip != ip {
		s.app.mu.Unlock()
		return "", nil, errApprovalNotFound
	}
	status := a.status
	if now.After(a.expires) {
		status = ApprovalExpired
	}
	if status != ApprovalPending {
		delete(s.app.approvals, a.id)
	}
	userID, username, device := a.userID, a.username, a.device
	s.app.mu.Unlock()
	if status != ApprovalApproved {
		return status, nil, nil
	}
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	if user.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.adminNetworkDenied(ctx, userID, username, ip, "password")
		return "", nil, errAdminNetwork()
	}
	cookie, err := s.NewSession(ctx, userID, ip)
	if err != nil {
		return "", nil, err
	}
	s.totpOK(userID)
	at, aid, an := userActor(userID, username)
	st, _, _, _ := s.q.SessionByToken(ctx, HashToken(cookie.Value))
	b, _ := json.Marshal(map[string]string{"second_factor": "phone", "device": device})
	s.loginEvent(ctx, audit.Event{Action: "Login", ActorType: at, ActorID: aid, ActorName: an, IP: ip, Session: audit.SessionTag(st.ID), Params: string(b)})
	s.checkNewNetwork(ctx, userID, username, user.Role, ip, RequestMeta(ctx).UserAgent)
	return ApprovalApproved, cookie, nil
}

// AppApprovals 列出等这个成员确认的请求。
func (s *Service) AppApprovals(u User) []PendingApproval {
	now := time.Now()
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	var out []PendingApproval
	for _, a := range s.app.approvals {
		if a.userID == u.ID && a.status == ApprovalPending && now.Before(a.expires) {
			out = append(out, PendingApproval{ID: a.id, Kind: a.kind, IP: a.ip, Agent: a.agent, Created: a.created, Expires: a.expires})
		}
	}
	slices.SortFunc(out, func(x, y PendingApproval) int { return x.Created.Compare(y.Created) })
	return out
}

// ApprovalSigned 是确认时签名的内容：用途、连接的主机、设备 ID、请求 ID、种类、数字，换行分隔。
func ApprovalSigned(host, deviceID, id, kind, code string) []byte {
	return []byte(strings.Join([]string{approvalPurpose, host, deviceID, id, kind, code}, "\n"))
}

// AppElevateSigned 是 App 做高权限确认时签名的内容。
func AppElevateSigned(host, deviceID, challenge string) []byte {
	return []byte(strings.Join([]string{appElevatePurpose, host, deviceID, challenge}, "\n"))
}

// AppApprovalRequest 是手机对一个请求的答复。
type AppApprovalRequest struct {
	ID, Code, Host string
	Deny           bool
	Signature      []byte
}

// AppApprove 确认或拒绝一个请求。确认要数字对、确认密钥的签名对；数字输错 approvalCodeTries 次请求作废。
// 拒绝登录请求时告警：有人知道这个账号的密码。
func (s *Service) AppApprove(ctx context.Context, ip string, u User, req AppApprovalRequest, hosts []string) error {
	if u.DeviceID == "" {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("只能从 App 调用"))
	}
	d, err := s.q.AppDeviceByID(ctx, u.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return errDeviceNotFound
	} else if err != nil {
		return err
	}
	at, aid, an := userActor(u.ID, u.Name)
	tag := audit.SessionTag("app:" + d.ID)
	event := func(action, kind, from, result string) {
		b, _ := json.Marshal(map[string]string{"kind": kind, "from": from})
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: action, ActorType: at, ActorID: aid, ActorName: an, IP: ip,
			Via: audit.ViaApp, Client: d.Name, Session: tag, Result: result, Params: string(b)})
	}
	now := time.Now()
	s.app.mu.Lock()
	a := s.app.approvals[req.ID]
	if a == nil || a.userID != u.ID || a.status != ApprovalPending || now.After(a.expires) {
		s.app.mu.Unlock()
		return errApprovalNotFound
	}
	kind, from := a.kind, a.ip
	if req.Deny {
		a.status = ApprovalDenied
		s.app.mu.Unlock()
		event("app_approval_denied", kind, from, audit.ResultDenied)
		if kind == ApprovalLogin && s.Alert != nil {
			s.Alert(context.WithoutCancel(ctx), "login-approval-denied:"+strconv.FormatInt(u.ID, 10), i18n.New("登录请求在手机上被拒绝").Error(),
				i18n.Errorf("账号 %s 的登录请求在手机 %s 上被拒绝。发起的人知道这个账号的密码，请尽快更改密码。来源：%s", u.Name, d.Name, from).Error())
		}
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(req.Code), []byte(a.code)) != 1 {
		a.tries++
		if a.tries >= approvalCodeTries {
			a.status = ApprovalDenied
		}
		s.app.mu.Unlock()
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("数字不正确"))
	}
	code := a.code
	s.app.mu.Unlock()
	if !slices.Contains(hosts, req.Host) && !s.devAppKeys() {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("连接的地址不是这个面板"))
	}
	pk, err := parseP256(d.ConfirmKey)
	if err != nil || !ecdsa.VerifyASN1(pk, sha256Sum(ApprovalSigned(req.Host, d.ID, req.ID, kind, code)), req.Signature) {
		event("app_signature_invalid", kind, from, audit.ResultDenied)
		return connect.NewError(connect.CodeUnauthenticated, i18n.New("签名不正确"))
	}
	s.app.mu.Lock()
	a = s.app.approvals[req.ID]
	if a == nil || a.status != ApprovalPending || time.Now().After(a.expires) {
		s.app.mu.Unlock()
		return errApprovalNotFound
	}
	a.status, a.device = ApprovalApproved, d.Name
	// 确认以后网页要在 approvalClaimTTL 内来取。
	a.expires = time.Now().Add(approvalClaimTTL)
	s.app.mu.Unlock()
	event("app_approved", kind, from, audit.ResultOK)
	return nil
}

// AppElevate 是 App 自己的高权限确认：确认密钥签 AppChallenge 给的挑战。记在这台手机上（内存里），15 分钟内有效，
// 只对确认时的 IP 有效。算 ElevatedByApp：给自己留登录凭据的操作不认。
func (s *Service) AppElevate(ctx context.Context, ip string, u User, challenge, host string, signature []byte, hosts []string) (time.Time, error) {
	if u.DeviceID == "" {
		return time.Time{}, connect.NewError(connect.CodePermissionDenied, i18n.New("只能从 App 调用"))
	}
	d, err := s.q.AppDeviceByID(ctx, u.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errDeviceNotFound
	} else if err != nil {
		return time.Time{}, err
	}
	if !s.useAppChallenge(d.ID, challenge) {
		return time.Time{}, connect.NewError(connect.CodeFailedPrecondition, i18n.New("挑战已过期，请重试"))
	}
	if !slices.Contains(hosts, host) && !s.devAppKeys() {
		return time.Time{}, connect.NewError(connect.CodePermissionDenied, i18n.New("连接的地址不是这个面板"))
	}
	at, aid, an := userActor(u.ID, u.Name)
	tag := audit.SessionTag("app:" + d.ID)
	pk, err := parseP256(d.ConfirmKey)
	if err != nil || !ecdsa.VerifyASN1(pk, sha256Sum(AppElevateSigned(host, d.ID, challenge)), signature) {
		b, _ := json.Marshal(map[string]string{"kind": "elevate"})
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_signature_invalid", ActorType: at, ActorID: aid, ActorName: an,
			IP: ip, Via: audit.ViaApp, Client: d.Name, Session: tag, Result: audit.ResultDenied, Params: string(b)})
		return time.Time{}, connect.NewError(connect.CodeUnauthenticated, i18n.New("签名不正确"))
	}
	until := time.Now().Add(ElevationTTL)
	s.app.mu.Lock()
	if s.app.elevated == nil {
		s.app.elevated = map[string]appElevation{}
	}
	s.app.elevated[d.ID] = appElevation{until: until, ip: ip}
	s.app.mu.Unlock()
	s.loginEvent(ctx, audit.Event{Action: "app_elevated", ActorType: at, ActorID: aid, ActorName: an, IP: ip, Via: audit.ViaApp,
		Client: d.Name, Session: tag})
	return until, nil
}

// appElevatedUntil 返回这台手机在这个 IP 上的高权限确认的到期时间（Unix 秒），没有或过期时为 0。
func (s *Service) appElevatedUntil(deviceID, ip string) int64 {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	e, ok := s.app.elevated[deviceID]
	if !ok || e.ip != ip || time.Now().After(e.until) {
		return 0
	}
	return e.until.Unix()
}
