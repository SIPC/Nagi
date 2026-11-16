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
)

type violations struct {
	a *agent

	mu          sync.Mutex
	pending     []*nagiv1.Violation
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
	v.a.audit.Record(auditlog.Event{Actor: "mesh:" + caller.String(), Action: "violation", Target: strings.Trim(projectID+"/"+container, "/"),
		Detail: method + " " + detail, Result: "denied"})
	v.pending = append(v.pending, &nagiv1.Violation{Caller: caller[:], Method: method, ProjectId: projectID, Container: container,
		Port: uint32(port), Detail: detail, Time: timestamppb.New(now)})
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
		v.mu.Lock()
		batch, dropped := v.pending, v.dropped
		v.pending, v.dropped = nil, 0
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
