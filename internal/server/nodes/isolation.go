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

// ReloadIsolation 从数据库重新加载隔离状态，并更新 mesh 成员。备用面板每次同步主面板的数据以后调用
// （见 docs/mesh.md#隔离）：否则主面板隔离或解除隔离的节点，备用面板这边一直不知道。
func (r *Registry) ReloadIsolation(ctx context.Context) {
	r.reloadIsolation(ctx)
	r.updateMesh()
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
// violationRecordLimit：每个上报的节点在 relayWindow 内最多记这么多条越权，超出的只计数，写进告警。上报的节点也可能被攻破，
// 不能借上报刷写审计和告警（见 docs/mesh.md#越权识别）。
const (
	violationAlertWindow = 10 * time.Minute
	violationRecordLimit = 200
	maxViolationWindows  = 1000
)

// ReportViolations 记下节点上报的越权请求。上报的节点可能被攻破、诬告别的节点，所以：
//   - 审计记录的操作者是上报的节点，被指控的一方记在对象里（target_type 是 node，没登记的公钥是 mesh:<公钥>）；
//   - 每个上报的节点 10 分钟内最多记 200 条，字段都截短；
//   - 只为登记过的节点按发起方单独告警，标题是「节点 X 报告 Y 越权」；没登记的公钥合并成一条按上报节点的告警；
//   - 被隔离的节点的上报只记录、不告警。
func (p *panelService) ReportViolations(ctx context.Context, req *nagiv1.ReportViolationsRequest) (*nagiv1.ReportViolationsResponse, error) {
	if p.conn.Pending() {
		return nil, status.Error(codes.PermissionDenied, "节点尚未批准")
	}
	list := req.GetViolations()
	if len(list) > 200 {
		list = list[:200]
	}
	allowed := p.r.allowViolations(p.conn.NodeID, len(list))
	dropped := int(req.GetDropped()) + len(list) - allowed
	list = list[:allowed]
	reporter := p.conn.Name()
	trusted := !p.r.Isolated(ctx, p.conn.NodeID)
	byOffender := map[string][]string{}
	names := map[string]string{}
	unknown := map[string]bool{}
	var unknownItems []string
	for _, v := range list {
		offenderID, offender, known := "", "mesh:"+clipText(fmt.Sprintf("%x", v.GetCaller()), 64), false
		if c, name, ok := p.r.NodeByMeshKey(v.GetCaller()); ok {
			offenderID, offender, known = c.NodeID, name, true
		} else if id, name, ok := p.r.nodeByStoredKey(ctx, v.GetCaller()); ok {
			offenderID, offender, known = id, name, true
		}
		method, project, container := clipText(v.GetMethod(), 64), clipText(v.GetProjectId(), 64), clipText(v.GetContainer(), 64)
		params, _ := json.Marshal(map[string]any{"reporter": reporter, "reporter_id": p.conn.NodeID, "offender": offender, "offender_id": offenderID,
			"method": method, "project": project, "container": container, "port": v.GetPort(), "detail": clipText(v.GetDetail(), 200)})
		r := p.r
		if r.Audit != nil {
			target := offenderID
			if target == "" {
				target = offender
			}
			r.Audit.Record(ctx, audit.Event{Category: audit.Security, Action: "violation", ActorType: audit.ActorNode, ActorID: p.conn.NodeID,
				ActorName: reporter, Via: audit.ViaNode, ProjectID: project, TargetType: "node", TargetID: target,
				TargetName: offender, Params: string(params), Result: audit.ResultDenied})
		}
		desc := method
		if project != "" {
			desc += " " + project
			if container != "" {
				desc += "/" + container
			}
			if v.GetPort() != 0 {
				desc += fmt.Sprintf(":%d", v.GetPort())
			}
		}
		if !known {
			unknown[offender] = true
			unknownItems = append(unknownItems, desc)
			continue
		}
		names[offenderID] = offender
		byOffender[offenderID] = append(byOffender[offenderID], desc)
	}
	if trusted {
		for id, items := range byOffender {
			p.r.violationAlert(ctx, "violation:"+id, i18n.Errorf("节点 %s 报告 %s 越权", reporter, names[id]).Error(), items, dropped)
		}
		if len(unknown) > 0 {
			p.r.violationAlert(ctx, "violation-unknown:"+p.conn.NodeID,
				i18n.Errorf("节点 %s 报告 %d 个未登记的 mesh 地址越权", reporter, len(unknown)).Error(), unknownItems, dropped)
		}
	}
	return &nagiv1.ReportViolationsResponse{}, nil
}

// allowViolations 返回这次上报里还能记几条（每个节点 relayWindow 内最多 violationRecordLimit 条）。
func (r *Registry) allowViolations(nodeID string, n int) int {
	r.relayMu.Lock()
	defer r.relayMu.Unlock()
	if r.violations == nil {
		r.violations = map[string][]time.Time{}
	}
	ok := 0
	for range n {
		if !allowWithin(r.violations, nodeID, violationRecordLimit) {
			break
		}
		ok++
	}
	return ok
}

// PanelViolation 记一条面板自己发现的越权（伪造面板指纹、乱报地址超限、代发告警超限等），标为「面板发现」。
func (r *Registry) PanelViolation(ctx context.Context, c *Conn, method, detail string) {
	params, _ := json.Marshal(map[string]string{"reporter": "panel", "method": method, "detail": detail})
	if r.Audit != nil {
		r.Audit.Record(ctx, audit.Event{Category: audit.Security, Action: "violation", ActorType: audit.ActorNode, ActorID: c.NodeID,
			ActorName: c.Name(), Via: audit.ViaNode, Params: string(params), Result: audit.ResultDenied})
	}
	r.violationAlert(ctx, "violation:"+c.NodeID, i18n.Errorf("节点 %s 疑似被攻破", c.Name()).Error(), []string{i18n.New("面板发现").Error() + "：" + method}, 0)
}

// violationAlert 同一个 key 10 分钟内只告警一次：列出前 5 条行为。
func (r *Registry) violationAlert(ctx context.Context, key, title string, items []string, dropped int) {
	now := time.Now()
	r.iso.mu.Lock()
	if r.iso.windows == nil {
		r.iso.windows = map[string]time.Time{}
	}
	if t, ok := r.iso.windows[key]; ok && now.Sub(t) < violationAlertWindow {
		r.iso.mu.Unlock()
		return
	}
	// 过期的窗口删掉，表不能一直变大。
	if len(r.iso.windows) >= maxViolationWindows {
		for k, t := range r.iso.windows {
			if now.Sub(t) >= violationAlertWindow {
				delete(r.iso.windows, k)
			}
		}
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
	msg := strings.Join(uniq, "；")
	if dropped > 0 {
		msg += "\n" + i18n.Errorf("另有 %d 条未逐条记录", dropped).Error()
	}
	r.PanelAlert(context.WithoutCancel(ctx), &nagiv1.Alert{Key: key, Title: title, Message: msg, Time: timestamppb.Now()})
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
