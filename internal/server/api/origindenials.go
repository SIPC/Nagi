package api

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 来源检查拒绝的请求（cross_origin_request）不用登录就能触发，逐条记的话没登录的人能把审计库写满，
// 挤掉普通记录（见 docs/audit.md#存储保留与同步「防止被刷」）。所以和登录失败一样汇总：每个 IP 每小时第一次单独记一条，
// 每小时最多单独记 originLogPerHour 条，其余只计数，整点写一条汇总；路径和来源截到 originClip 字节。

const (
	originLogPerHour = 100
	originMaxIPs     = 1000
	originClip       = 256
)

type originDenials struct {
	mu     sync.Mutex
	hour   int64
	ips    map[string]int // IP → 单独记下之外的次数
	logged int
	// other 是 ips 满了以后其他 IP 的次数。
	other int
}

// denyOrigin 记一次被来源检查拒绝的请求。
func (a *API) denyOrigin(ctx context.Context, ip, path, origin, site string) {
	d := &a.origins
	now := time.Now().Unix() / 3600
	d.mu.Lock()
	var pending map[string]int
	var pendingOther int
	if now != d.hour {
		pending, pendingOther = d.ips, d.other
		d.hour, d.ips, d.logged, d.other = now, map[string]int{}, 0, 0
	}
	n, seen := d.ips[ip]
	logIt := false
	switch {
	case seen:
		d.ips[ip] = n + 1
	case d.logged < originLogPerHour:
		d.ips[ip] = 0
		d.logged++
		logIt = true
	case len(d.ips) < originMaxIPs:
		d.ips[ip] = 1
	default:
		d.other++
	}
	d.mu.Unlock()
	a.flushOrigins(ctx, pending, pendingOther)
	if logIt {
		params, _ := json.Marshal(map[string]string{"path": clipBytes(path, originClip), "origin": clipBytes(origin, originClip), "site": clipBytes(site, 64)})
		a.Record(ctx, audit.Event{Category: audit.Security, Action: "cross_origin_request", Result: audit.ResultDenied, Params: string(params)})
	}
}

// OriginDenialsLoop 每 10 分钟检查一次，过了整点就写出上一个小时的汇总。
func (a *API) OriginDenialsLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.FlushOriginDenials(ctx)
		}
	}
}

// FlushOriginDenials 写出上一个小时的汇总。
func (a *API) FlushOriginDenials(ctx context.Context) {
	d := &a.origins
	d.mu.Lock()
	if now := time.Now().Unix() / 3600; now == d.hour {
		d.mu.Unlock()
		return
	}
	pending, other := d.ips, d.other
	d.hour, d.ips, d.logged, d.other = time.Now().Unix()/3600, map[string]int{}, 0, 0
	d.mu.Unlock()
	a.flushOrigins(ctx, pending, other)
}

// flushOrigins 写一条汇总：总次数、涉及的 IP 数和次数最多的 10 个 IP。没有需要汇总的次数时不写。
func (a *API) flushOrigins(ctx context.Context, pending map[string]int, other int) {
	total := other
	ips := make([]string, 0, len(pending))
	for ip, n := range pending {
		total += n
		if n > 0 {
			ips = append(ips, ip)
		}
	}
	if total == 0 {
		return
	}
	sort.Slice(ips, func(i, j int) bool {
		if pending[ips[i]] != pending[ips[j]] {
			return pending[ips[i]] > pending[ips[j]]
		}
		return ips[i] < ips[j]
	})
	if len(ips) > 10 {
		ips = ips[:10]
	}
	params, _ := json.Marshal(map[string]any{"count": total, "ip_count": len(pending), "top_ips": ips})
	a.Record(auth.WithMeta(context.WithoutCancel(ctx), auth.Meta{}), audit.Event{Category: audit.Security, Action: "cross_origin_summary",
		ActorType: audit.ActorSystem, Result: audit.ResultDenied, Params: string(params)})
}

// clipBytes 截到最多 n 字节，不切断 UTF-8 字符。
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
