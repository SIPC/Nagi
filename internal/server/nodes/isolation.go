package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 节点越权识别和一键隔离（见 docs/mesh.md#越权识别、#隔离）。

// isolation 记着被隔离的节点（ID → mesh 公钥），从数据库加载，隔离状态随备用面板同步。
type isolation struct {
	mu     sync.RWMutex
	loaded bool
	nodes  map[string][]byte
	// windows 是每个发起方 10 分钟的告警窗口。
	windows map[string]time.Time
}

func (r *Registry) isolatedNodes(ctx context.Context) map[string][]byte {
	r.iso.mu.RLock()
	if r.iso.loaded {
		out := r.iso.nodes
		r.iso.mu.RUnlock()
		return out
	}
	r.iso.mu.RUnlock()
	r.reloadIsolation(ctx)
	r.iso.mu.RLock()
	defer r.iso.mu.RUnlock()
	return r.iso.nodes
}

func (r *Registry) reloadIsolation(ctx context.Context) {
	m := map[string][]byte{}
	if r.q == nil {
		r.iso.mu.Lock()
		r.iso.nodes, r.iso.loaded = m, true
		r.iso.mu.Unlock()
		return
	}
	if ids, keys, err := r.q.IsolatedNodes(ctx); err == nil {
		for i, id := range ids {
			k, _ := mesh.ParseKey(keys[i])
			m[id] = k[:]
		}
	}
	r.iso.mu.Lock()
	r.iso.nodes, r.iso.loaded = m, true
	r.iso.mu.Unlock()
}

// Isolated 判断节点有没有被隔离。
func (r *Registry) Isolated(ctx context.Context, nodeID string) bool {
	_, ok := r.isolatedNodes(ctx)[nodeID]
	return ok
}

// SetIsolated 隔离或解除隔离节点：立即从所有节点的 mesh 成员里去掉（不走保留期），停止为它中转；
// 撤销它在代理计划里的项目放行由 OnIsolate（面板重算代理计划）完成。它的控制连接保留，容器不停。
func (r *Registry) SetIsolated(ctx context.Context, nodeID string, on bool) error {
	if err := r.q.SetNodeIsolated(ctx, nodeID, on); err != nil {
		return err
	}
	r.reloadIsolation(ctx)
	r.updateMesh()
	r.pushMesh(ctx)
	// 隔离时让节点删掉外送目标（里面有凭据），解除时重新下发。
	if c, err := r.Peek(nodeID); err == nil {
		go r.pushAuditSinksTo(c)
	}
	if r.OnIsolate != nil {
		r.OnIsolate(ctx)
	}
	return nil
}

// pushMesh 立即把新的成员列表推给所有在线、没被隔离的节点（平时随上报的回复下发，最多晚 10 秒）。
func (r *Registry) pushMesh(ctx context.Context) {
	iso := r.isolatedNodes(ctx)
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		if _, bad := iso[c.NodeID]; !bad {
			conns = append(conns, c)
		}
	}
	r.mu.RUnlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		cfg := r.meshConfig(c)
		if cfg == nil || c.Agent == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := c.Agent.SetMesh(cctx, cfg); err != nil && status.Code(err) != codes.Unimplemented {
				r.log.Warn("推送 mesh 成员失败", "node", c.NodeID, "err", err)
			}
		}()
	}
	wg.Wait()
}

// isolatedKeys 是 MeshConfig.isolated：被隔离的节点的 mesh 公钥。
func (r *Registry) isolatedKeys() [][]byte {
	var out [][]byte
	for _, k := range r.isolatedNodes(context.Background()) {
		if len(k) > 0 {
			out = append(out, k)
		}
	}
	return out
}

// ---- 越权上报 ----

// violationAlertWindow：同一个发起方这么久之内的越权汇总成一条告警。
const violationAlertWindow = 10 * time.Minute

// ReportViolations 记下节点上报的越权请求：发起方记为审计记录的操作者，上报的节点记在详情里。同一个发起方 10 分钟内
// 汇总成一条告警。被隔离的节点的上报只记录、不告警（它可能诬告别的节点）。
func (p *panelService) ReportViolations(ctx context.Context, req *nagiv1.ReportViolationsRequest) (*nagiv1.ReportViolationsResponse, error) {
	if p.conn.Pending() {
		return nil, status.Error(codes.PermissionDenied, "节点尚未批准")
	}
	list := req.GetViolations()
	if len(list) > 200 {
		list = list[:200]
	}
	reporter := p.conn.Name()
	trusted := !p.r.Isolated(ctx, p.conn.NodeID)
	byOffender := map[string][]string{}
	names := map[string]string{}
	for _, v := range list {
		offenderID, offender := "", fmt.Sprintf("%x", v.GetCaller())
		if c, name, ok := p.r.NodeByMeshKey(v.GetCaller()); ok {
			offenderID, offender = c.NodeID, name
		} else if id, name, ok := p.r.nodeByStoredKey(ctx, v.GetCaller()); ok {
			offenderID, offender = id, name
		}
		params, _ := json.Marshal(map[string]any{"reporter": reporter, "reporter_id": p.conn.NodeID, "method": v.GetMethod(),
			"project": v.GetProjectId(), "container": v.GetContainer(), "port": v.GetPort(), "detail": clipText(v.GetDetail(), 200)})
		r := p.r
		if r.Audit != nil {
			r.Audit.Record(ctx, audit.Event{Category: audit.Security, Action: "violation", ActorType: audit.ActorNode, ActorID: offenderID,
				ActorName: offender, Via: audit.ViaNode, ProjectID: v.GetProjectId(), TargetType: "node", TargetID: p.conn.NodeID,
				TargetName: reporter, Params: string(params), Result: audit.ResultDenied})
		}
		key := offenderID
		if key == "" {
			key = offender
		}
		names[key] = offender
		desc := v.GetMethod()
		if v.GetProjectId() != "" {
			desc += " " + v.GetProjectId()
			if v.GetContainer() != "" {
				desc += "/" + v.GetContainer()
			}
			if v.GetPort() != 0 {
				desc += fmt.Sprintf(":%d", v.GetPort())
			}
		}
		byOffender[key] = append(byOffender[key], desc)
	}
	if trusted {
		for key, items := range byOffender {
			p.r.violationAlert(ctx, key, names[key], items, i18n.Errorf("节点 %s 报告", reporter).Error(), int(req.GetDropped()))
		}
	}
	return &nagiv1.ReportViolationsResponse{}, nil
}

// PanelViolation 记一条面板自己发现的越权（伪造面板指纹、乱报地址超限、代发告警超限等），标为「面板发现」。
func (r *Registry) PanelViolation(ctx context.Context, c *Conn, method, detail string) {
	params, _ := json.Marshal(map[string]string{"reporter": "panel", "method": method, "detail": detail})
	if r.Audit != nil {
		r.Audit.Record(ctx, audit.Event{Category: audit.Security, Action: "violation", ActorType: audit.ActorNode, ActorID: c.NodeID,
			ActorName: c.Name(), Via: audit.ViaNode, Params: string(params), Result: audit.ResultDenied})
	}
	r.violationAlert(ctx, c.NodeID, c.Name(), []string{method}, i18n.New("面板发现").Error(), 0)
}

// violationAlert 同一个发起方 10 分钟内只告警一次：列出前 5 条行为和来源。
func (r *Registry) violationAlert(ctx context.Context, key, name string, items []string, source string, dropped int) {
	now := time.Now()
	r.iso.mu.Lock()
	if r.iso.windows == nil {
		r.iso.windows = map[string]time.Time{}
	}
	if t, ok := r.iso.windows[key]; ok && now.Sub(t) < violationAlertWindow {
		r.iso.mu.Unlock()
		return
	}
	r.iso.windows[key] = now
	r.iso.mu.Unlock()
	sort.Strings(items)
	uniq := items[:0]
	for i, s := range items {
		if i == 0 || s != items[i-1] {
			uniq = append(uniq, s)
		}
	}
	if len(uniq) > 5 {
		uniq = uniq[:5]
	}
	msg := source + "：" + strings.Join(uniq, "；")
	if dropped > 0 {
		msg += "\n" + i18n.Errorf("另有 %d 条未逐条上报", dropped).Error()
	}
	r.PanelAlert(context.WithoutCancel(ctx), &nagiv1.Alert{Key: "violation:" + key, Title: i18n.Errorf("节点 %s 疑似被攻破", name).Error(), Message: msg, Time: timestamppb.Now()})
}

// nodeByStoredKey 按数据库里登记的 mesh 公钥找节点（离线的节点）。
func (r *Registry) nodeByStoredKey(ctx context.Context, key []byte) (string, string, bool) {
	k, err := mesh.ParseKey(key)
	if err != nil {
		return "", "", false
	}
	rows, err := r.q.ListNodes(ctx)
	if err != nil {
		return "", "", false
	}
	for _, n := range rows {
		if bytes.Equal(n.MeshKey, k[:]) {
			return n.ID, n.Name, true
		}
	}
	return "", "", false
}
