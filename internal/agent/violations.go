package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/auditlog"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 越权识别（见 docs/mesh.md#越权识别）：mesh 上每个请求都由 WireGuard 认证了来源，正常节点从不发出越权请求，
// 所以被拒绝的请求说明发起方很可能被攻破了。节点把拒绝记下来，每 10 秒一批经控制连接告诉面板，
// 每 10 分钟最多上报 200 条，超出的只计数。由面板判断、告警，节点自己不处置。

const (
	violationFlush  = 10 * time.Second
	violationWindow = 10 * time.Minute
	violationLimit  = 200
	// revokeGrace：放行刚撤销的这段时间里，已有连接的重连不算越权（断开项目连接、移除节点、成员列表的保留期）。
	revokeGrace = 2 * time.Minute
	// violationHold：记下的越权先等这么久再上报，到时按现在的配置再查一次，已经允许了就不报。
	// 面板改了放行（新连接了项目、容器换了节点、副本换了源节点）时，各节点拿到新配置有先有后，
	// 先拿到的一方发起的请求会被还没拿到的一方拒绝，这不是越权。
	violationHold = 30 * time.Second
)

// pendingViolation 是一条等着上报的越权。still 为空表示不用再查；不为空时上报前调用，返回 false 说明现在已经允许了。
type pendingViolation struct {
	v     *nagiv1.Violation
	ev    auditlog.Event
	at    time.Time
	still func() bool
}

type violations struct {
	a *agent

	mu          sync.Mutex
	pending     []pendingViolation
	dropped     uint32
	windowStart time.Time
	inWindow    int
	// revoked 记着最近被撤销放行的调用方和时间。
	revoked map[mesh.Key]time.Time
}

func newViolations(a *agent) *violations {
	return &violations{a: a, revoked: map[mesh.Key]time.Time{}}
}

// revoke 记下调用方 k 的放行刚被撤销，2 分钟内它的请求被拒绝不算越权。
func (v *violations) revoke(k mesh.Key) {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.revoked[k] = time.Now()
	v.mu.Unlock()
}

// add 记下一次被拒绝的越权请求。
func (v *violations) add(caller mesh.Key, method, projectID, container string, port uint16, detail string) {
	v.addCheck(caller, method, projectID, container, port, detail, nil)
}

// addCheck 和 add 一样，still 在上报前按当时的配置再查一次（见 violationHold）。
func (v *violations) addCheck(caller mesh.Key, method, projectID, container string, port uint16, detail string, still func() bool) {
	if v == nil {
		return
	}
	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if t, ok := v.revoked[caller]; ok {
		if now.Sub(t) < revokeGrace {
			return
		}
		delete(v.revoked, caller)
	}
	if now.Sub(v.windowStart) >= violationWindow {
		v.windowStart, v.inWindow = now, 0
	}
	if v.inWindow >= violationLimit {
		v.dropped++
		return
	}
	v.inWindow++
	v.pending = append(v.pending, pendingViolation{
		v: &nagiv1.Violation{Caller: caller[:], Method: method, ProjectId: projectID, Container: container,
			Port: uint32(port), Detail: detail, Time: timestamppb.New(now)},
		ev: auditlog.Event{Actor: "mesh:" + caller.String(), Action: "violation", Target: strings.Trim(projectID+"/"+container, "/"),
			Detail: method + " " + detail, Result: "denied"},
		at: now, still: still,
	})
}

// ready 取出已经等够 violationHold 的越权，按现在的配置再查一次，仍然不允许的记进本地审计并返回。
func (v *violations) ready(now time.Time) []*nagiv1.Violation {
	v.mu.Lock()
	var due, keep []pendingViolation
	for _, p := range v.pending {
		if now.Sub(p.at) >= violationHold {
			due = append(due, p)
		} else {
			keep = append(keep, p)
		}
	}
	v.pending = keep
	v.mu.Unlock()
	var out []*nagiv1.Violation
	for _, p := range due {
		if p.still != nil && !p.still() {
			continue
		}
		if v.a.audit != nil {
			v.a.audit.Record(p.ev)
		}
		out = append(out, p.v)
	}
	return out
}

// run 每 10 秒把攒下的一批告诉所有连着的面板（面板据此告警；备用面板也收，只记录）。
func (v *violations) run(ctx context.Context) {
	t := time.NewTicker(violationFlush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		batch := v.ready(time.Now())
		v.mu.Lock()
		dropped := v.dropped
		v.dropped = 0
		v.mu.Unlock()
		if len(batch) == 0 && dropped == 0 {
			continue
		}
		v.a.mu.Lock()
		list := make([]nagiv1.PanelServiceClient, 0, len(v.a.panels))
		for _, c := range v.a.panels {
			list = append(list, c)
		}
		v.a.mu.Unlock()
		for _, c := range list {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, _ = c.ReportViolations(cctx, &nagiv1.ReportViolationsRequest{Violations: batch, Dropped: dropped})
			cancel()
		}
	}
}
