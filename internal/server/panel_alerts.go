package server

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// runPanelAlerts 判断面板自己的规则（见 docs/monitoring.md#告警），只在主面板上运行：
//   - 面板日志库的写入失败和容量（节点的在节点上判断）；
//   - 每天一次审计链校验；
//   - 访问令牌快到期（默认关闭）。
func (s *Server) runPanelAlerts(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastErrors uint64
	var errorTimes []time.Time
	seen := false
	var lastVerify time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if logs := s.cfg.Logs; logs != nil {
			if st, err := logs.State(ctx); err == nil {
				if seen && st.WriteErrors > lastErrors {
					for range min(st.WriteErrors-lastErrors, 1000) {
						errorTimes = append(errorTimes, now)
					}
				}
				lastErrors, seen = st.WriteErrors, true
				s.panelLogAlerts(ctx, now, &errorTimes, st.DiskBytes, st.BudgetBytes)
			}
		}
		if now.Sub(lastVerify) >= 24*time.Hour && s.audit != nil {
			lastVerify = now
			if res, err := s.audit.Verify(ctx); err == nil {
				msg := ""
				if !res.OK() {
					msg = "#" + strconv.FormatInt(res.BadID, 10) + " " + res.Reason
				}
				s.nodes.EvaluatePanelRule(ctx, nodes.PanelObservation{Key: "audit_verify", Rule: alert.RuleAuditVerify, Bad: !res.OK(), Good: res.OK(), Message: msg}, now)
			}
		}
		s.tokenAlerts(ctx, now)
	}
}

// panelLogAlerts：写入失败按窗口数次数；容量按 disk/budget。
func (s *Server) panelLogAlerts(ctx context.Context, now time.Time, times *[]time.Time, disk, budget int64) {
	policy, err := s.nodes.AlertPolicy(ctx)
	if err != nil {
		return
	}
	rules := alert.Merge(policy.GetRules())
	w := rules[alert.RuleLogWrite]
	window := time.Duration(max(1, w.GetWindowMinutes())) * time.Minute
	quiet := time.Duration(w.GetRecoverSeconds()) * time.Second
	kept := (*times)[:0]
	n := 0
	for _, t := range *times {
		if now.Sub(t) < max(window, quiet) {
			kept = append(kept, t)
			if now.Sub(t) < window {
				n++
			}
		}
	}
	*times = kept
	v := float64(n)
	good := len(kept) == 0 || now.Sub(kept[len(kept)-1]) >= quiet
	s.nodes.EvaluatePanelRule(ctx, nodes.PanelObservation{Key: "logstore_write:panel", Rule: alert.RuleLogWrite, Node: "panel", Value: &v,
		Bad: v >= w.GetThreshold(), Good: good}, now)
	if budget > 0 {
		f := rules[alert.RuleLogFull]
		pct := math.Min(100, 100*float64(disk)/float64(budget))
		s.nodes.EvaluatePanelRule(ctx, nodes.PanelObservation{Key: "logstore_full:panel", Rule: alert.RuleLogFull, Node: "panel", Value: &pct,
			Bad: pct > f.GetThreshold(), Good: alert.Def{}.Recovered(pct, f.GetRecoverThreshold())}, now)
	}
}

// tokenAlerts：个人访问令牌在 threshold 天内到期时提醒一次（重复提醒间隔默认 0）；续期或删除后结束。
func (s *Server) tokenAlerts(ctx context.Context, now time.Time) {
	policy, err := s.nodes.AlertPolicy(ctx)
	if err != nil {
		return
	}
	rule := alert.Merge(policy.GetRules())[alert.RuleTokenExpiry]
	tokens, err := s.q.ListAllAccessTokens(ctx)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, t := range tokens {
		if t.ExpiresAt == 0 || t.ClientID != "" { // 不过期的令牌；OAuth 授权靠刷新续期
			continue
		}
		key := "token_expiry:" + strconv.FormatInt(t.ID, 10)
		live[key] = true
		days := math.Floor(time.Unix(t.ExpiresAt, 0).Sub(now).Hours() / 24)
		s.nodes.EvaluatePanelRule(ctx, nodes.PanelObservation{Key: key, Rule: alert.RuleTokenExpiry, Subject: t.Name, Value: &days,
			Bad: days < rule.GetThreshold() && days >= 0, Good: days >= rule.GetThreshold()}, now)
	}
	for _, key := range s.nodes.PanelRuleKeys("token_expiry:") {
		if !live[key] {
			s.nodes.EndPanelRule(ctx, key)
		}
	}
}
