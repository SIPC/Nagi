package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// ListAlertBaselines 列出各节点上报的基线建立进度（见 docs/monitoring.md#基线），日志页「当前告警」下方显示。
// 权限和当前告警一样：要「审计日志」或「告警规则」；容器、映射的按项目范围（还要「查看」），节点的只给能看全部项目、
// 节点在范围内的成员。看不到的项目、节点不出现，也不暴露它们存在。没上报过（v0.37）的节点给一条 unsupported。
func (s *alertService) ListAlertBaselines(ctx context.Context, _ *connect.Request[nagiv1.ListAlertBaselinesRequest]) (*connect.Response[nagiv1.ListAlertBaselinesResponse], error) {
	u := auth.CurrentUser(ctx)
	if !u.Can(auth.PermAudit) && !u.Can(auth.PermAlerts) {
		return nil, permissionDenied()
	}
	out := &nagiv1.ListAlertBaselinesResponse{}
	if s.Nodes == nil {
		return connect.NewResponse(out), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	x, err := s.loadAccess(ctx, u)
	if err != nil {
		return nil, err
	}
	nodes, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	projectNames := map[string]string{}
	projectName := func(id string) string {
		if n, ok := projectNames[id]; ok {
			return n
		}
		n := ""
		if p, err := s.Q.GetProject(ctx, id); err == nil {
			n = p.Name
		}
		projectNames[id] = n
		return n
	}
	allProjects := x == nil || x.projects == nil
	for _, n := range nodes {
		nodeVisible := x == nil || x.node(n.ID)
		name := ""
		if nodeVisible {
			name = n.Name
		}
		list, _, reported, err := s.Nodes.AlertBaselines(ctx, n.ID)
		if err != nil {
			return nil, toConnect(err)
		}
		if !reported {
			// 只告诉能看这个节点的成员（项目成员看不出节点上还有别的什么）。
			if nodeVisible && allProjects {
				out.Baselines = append(out.Baselines, &nagiv1.AlertBaselineState{NodeId: n.ID, Node: name, Unsupported: true})
			}
			continue
		}
		for _, b := range list {
			v := &nagiv1.AlertBaselineState{Node: name, Baseline: b}
			if nodeVisible {
				v.NodeId = n.ID
			}
			if b.GetProjectId() != "" {
				if x != nil && !(x.project(b.GetProjectId()) && x.can(auth.PermView)) {
					continue
				}
				v.Project = projectName(b.GetProjectId())
				if v.Project == "" {
					continue // 项目已删除
				}
			} else if !(nodeVisible && allProjects) {
				continue
			}
			out.Baselines = append(out.Baselines, v)
		}
	}
	return connect.NewResponse(out), nil
}
