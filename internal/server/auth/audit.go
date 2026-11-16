package auth

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/server/audit"
)

// 登录相关的审计记录（类别 login，见 docs/audit.md#记录范围）。

// SetAudit 设置审计日志。
func (s *Service) SetAudit(l *audit.Log) { s.audit = l }

// loginEvent 写一条登录记录，来源从 ctx 里取。
func (s *Service) loginEvent(ctx context.Context, e audit.Event) {
	if s.audit == nil {
		return
	}
	m := RequestMeta(ctx)
	if e.Category == "" {
		e.Category = audit.Login
	}
	if e.Via == "" {
		e.Via = audit.ViaWeb
	}
	if e.IP == "" {
		e.IP = m.IP
	}
	e.UserAgent = m.UserAgent
	s.audit.Record(ctx, e)
}

func userActor(id int64, name string) (string, string, string) {
	return audit.ActorUser, strconv.FormatInt(id, 10), name
}

// failureSummary 按 IP 汇总登录失败（见 docs/audit.md#存储保留与同步「防止被刷」）：每个 IP 每小时第一次失败单独记一条，
// 之后的只计数，整点写一条汇总。
type failureSummary struct {
	mu   sync.Mutex
	hour int64
	ips  map[string]*ipFailures
}

type ipFailures struct {
	count     int
	usernames map[string]int
}

// recordLoginFailure 记一次用户名或密码错误。
func (s *Service) recordLoginFailure(ctx context.Context, ip, username string) {
	now := time.Now()
	f := &s.failures
	f.mu.Lock()
	if h := now.Unix() / 3600; h != f.hour {
		pending := f.ips
		f.hour, f.ips = h, map[string]*ipFailures{}
		f.mu.Unlock()
		s.flushFailures(ctx, pending)
		f.mu.Lock()
	}
	x, ok := f.ips[ip]
	if !ok {
		x = &ipFailures{usernames: map[string]int{}}
		f.ips[ip] = x
	}
	first := !ok
	if !first {
		x.count++
		if len(x.usernames) < 100 {
			x.usernames[clipName(username)]++
		}
	}
	f.mu.Unlock()
	if first {
		params, _ := json.Marshal(map[string]string{"username": clipName(username)})
		s.loginEvent(ctx, audit.Event{Action: "login_failed", ActorType: audit.ActorUser, ActorName: clipName(username), IP: ip,
			Params: string(params), Result: audit.ResultFailed, Error: ErrBadCredentials.Error()})
	}
}

// FlushLoginFailures 写出上一个小时的汇总，整点由 CleanupLoop 调用。
func (s *Service) FlushLoginFailures(ctx context.Context) {
	f := &s.failures
	f.mu.Lock()
	if h := time.Now().Unix() / 3600; h == f.hour {
		f.mu.Unlock()
		return
	}
	pending := f.ips
	f.hour, f.ips = time.Now().Unix()/3600, map[string]*ipFailures{}
	f.mu.Unlock()
	s.flushFailures(ctx, pending)
}

func (s *Service) flushFailures(ctx context.Context, pending map[string]*ipFailures) {
	ips := make([]string, 0, len(pending))
	for ip, x := range pending {
		if x.count > 0 {
			ips = append(ips, ip)
		}
	}
	sort.Strings(ips)
	for _, ip := range ips {
		x := pending[ip]
		names := make([]string, 0, len(x.usernames))
		for n := range x.usernames {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool {
			if x.usernames[names[i]] != x.usernames[names[j]] {
				return x.usernames[names[i]] > x.usernames[names[j]]
			}
			return names[i] < names[j]
		})
		if len(names) > 5 {
			names = names[:5]
		}
		params, _ := json.Marshal(map[string]any{"count": x.count, "usernames": names})
		if s.audit != nil {
			s.audit.Record(ctx, audit.Event{Category: audit.Login, Action: "login_failed_summary", ActorType: audit.ActorSystem,
				Via: audit.ViaWeb, IP: ip, Params: string(params), Result: audit.ResultFailed})
		}
	}
}

func clipName(v string) string {
	r := []rune(v)
	if len(r) > 64 {
		return string(r[:64])
	}
	return v
}
