package auth

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
)

// 登录相关的安全告警（见 docs/audit.md#安全告警），在「设置 → 告警」里有开关，默认开启：
//   - 密码正确但验证码连续错误 5 次，或者登录的手机确认连续 5 次没有通过；
//   - 按 IP 的登录限速开始等待，每小时汇总一条；
//   - 管理员从新的网段登录（最近 90 天没有登录过的 IPv4 /24、IPv6 /48）。

// 以前开关的设置项（"0" 表示关闭）。v0.37.1 起开关是 设置 → 告警 的事件通知（见 alert.Notices），
// 升级时迁移过去（nodes.migrateLoginNotices），旧设置项保留不用。
const (
	SettingAlertTOTP       = "alert_totp_failures"
	SettingAlertRateLimit  = "alert_login_ratelimit"
	SettingAlertNewNetwork = "alert_admin_network"
)

// 对应的事件通知。
const (
	NoticeTOTP       = "login_totp"
	NoticeRateLimit  = "login_ratelimit"
	NoticeNewNetwork = "login_new_network"
)

const (
	totpAlertAfter = 5
	networkMemory  = 90 * 24 * time.Hour
)

// AlertEnabled 判断一项登录相关的事件通知有没有开（NoticeEnabled 没有设置时都算开着）。
func (s *Service) AlertEnabled(ctx context.Context, notice string) bool {
	return s.NoticeEnabled == nil || s.NoticeEnabled(ctx, notice)
}

type totpStreaks struct {
	mu sync.Mutex
	n  map[int64]int
	// phone 是登录的手机确认连续没有通过（过期没人确认、数字输错作废）的次数。
	phone map[int64]int
}

// totpFailed 记一次验证码错误；连续第 5 次时告警（之后不重复，直到验证成功）。
func (s *Service) totpFailed(ctx context.Context, userID int64, username, ip string) {
	s.streaks.mu.Lock()
	if s.streaks.n == nil {
		s.streaks.n = map[int64]int{}
	}
	s.streaks.n[userID]++
	n := s.streaks.n[userID]
	s.streaks.mu.Unlock()
	if n == totpAlertAfter && s.Alert != nil && s.AlertEnabled(ctx, NoticeTOTP) {
		at, aid, an := userActor(userID, username)
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "totp_failures", ActorType: at, ActorID: aid, ActorName: an, IP: ip, Result: audit.ResultDenied})
		s.Alert(context.WithoutCancel(ctx), "totp:"+strconv.FormatInt(userID, 10), i18n.New("登录安全").Error(),
			i18n.Errorf("账号 %s 的密码可能已泄露：密码正确，但验证码连续错误 %d 次。来源 IP：%s", username, totpAlertAfter, ip).Error())
	}
}

// loginApprovalFailed 记一次登录的手机确认没有通过；连续第 5 次时告警（之后不重复，直到登录成功）。
// 和验证码连续错误是同一种情况（密码对了，第二步过不去），用同一个开关。
func (s *Service) loginApprovalFailed(ctx context.Context, userID int64, username, ip string) {
	s.streaks.mu.Lock()
	if s.streaks.phone == nil {
		s.streaks.phone = map[int64]int{}
	}
	s.streaks.phone[userID]++
	n := s.streaks.phone[userID]
	s.streaks.mu.Unlock()
	if n == totpAlertAfter && s.Alert != nil && s.AlertEnabled(ctx, NoticeTOTP) {
		at, aid, an := userActor(userID, username)
		s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "login_approval_failures", ActorType: at, ActorID: aid, ActorName: an, IP: ip, Result: audit.ResultDenied})
		s.Alert(context.WithoutCancel(ctx), "login-approval:"+strconv.FormatInt(userID, 10), i18n.New("登录安全").Error(),
			i18n.Errorf("账号 %s 的密码可能已泄露：密码正确，但登录的手机确认连续 %d 次没有通过。来源 IP：%s", username, totpAlertAfter, ip).Error())
	}
}

func (s *Service) totpOK(userID int64) {
	s.streaks.mu.Lock()
	delete(s.streaks.n, userID)
	delete(s.streaks.phone, userID)
	s.streaks.mu.Unlock()
}

// rateLimited 记下这个 IP 在这个小时里碰到了登录限速，整点汇总时告警。
func (s *Service) rateLimited(ip string) {
	f := &s.failures
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limited == nil {
		f.limited = map[string]bool{}
	}
	f.limited[ip] = true
}

// alertRateLimited 在写出上一个小时的汇总时调用：碰到限速的 IP 数、失败次数、被尝试最多的用户名。
func (s *Service) alertRateLimited(ctx context.Context, limited map[string]bool, pending map[string]*ipFailures) {
	if len(limited) == 0 || s.Alert == nil || !s.AlertEnabled(ctx, NoticeRateLimit) {
		return
	}
	failures := 0
	names := map[string]int{}
	for ip := range limited {
		if x := pending[ip]; x != nil {
			failures += x.count + 1
			for n, c := range x.usernames {
				names[n] += c
			}
		}
	}
	top := topNames(names, 5)
	msg := i18n.Errorf("上一小时 %d 个 IP 因登录失败次数过多被限速，共失败 %d 次", len(limited), failures).Error()
	if len(top) > 0 {
		msg += "\n" + i18n.Errorf("被尝试的用户名：%s", strings.Join(top, "、")).Error()
	}
	s.Alert(context.WithoutCancel(ctx), "ratelimit:"+strconv.FormatInt(time.Now().Unix()/3600, 10), i18n.New("登录安全").Error(), msg)
}

func topNames(m map[string]int, n int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if m[out[j]] > m[out[i]] || m[out[j]] == m[out[i]] && out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// checkNewNetwork 记下成员登录的网段；管理员从最近 90 天没有登录过的网段登录时告警。成员第一次登录（还没有任何记录）不告警。
func (s *Service) checkNewNetwork(ctx context.Context, userID int64, username, role, ip, ua string) {
	_, network := Network(ip)
	if network == "" {
		return
	}
	now := time.Now()
	seen, any, err := s.q.SeenNetwork(ctx, userID, network, now.Unix(), now.Add(-networkMemory).Unix())
	if err != nil || seen || !any || role != RoleAdmin || s.Alert == nil || !s.AlertEnabled(ctx, NoticeNewNetwork) {
		return
	}
	at, aid, an := userActor(userID, username)
	s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "admin_new_network", ActorType: at, ActorID: aid, ActorName: an, IP: ip,
		Params: `{"network":"` + network + `"}`})
	s.Alert(context.WithoutCancel(ctx), "network:"+strconv.FormatInt(userID, 10)+":"+network, i18n.New("登录安全").Error(),
		i18n.Errorf("管理员 %s 从新的网段登录。IP：%s；浏览器：%s", username, ip, ParseAgent(ua).String()).Error())
}
