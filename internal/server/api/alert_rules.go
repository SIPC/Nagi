package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 设置 → 告警 里的规则（见 docs/monitoring.md#告警）。权限沿用以前的：
//   - 全局默认值、通知语言、抖动保护：面板设置和告警渠道（panel.settings）；
//   - 节点的覆盖：节点管理（node.manage），节点在访问范围内；
//   - 项目的覆盖：项目的告警规则（project.alerts）或项目管理（project.manage）；容器的覆盖：project.alerts；项目在访问范围内。
// 读取时只返回调用者能管的覆盖，看不到别的项目、节点上的设置（也不暴露它们存在）。

// canEditOverride 判断成员能不能看、改这条覆盖。x 为 nil 是管理员。
func canEditOverride(u auth.User, x *access, o *nagiv1.AlertOverride) bool {
	switch o.GetScope() {
	case "node":
		return u.Can(auth.PermNodeManage) && x.nodeInScope(o.GetNodeId())
	case "project":
		return (u.Can(auth.PermAlerts) || u.Can(auth.PermProjectManage)) && x.project(o.GetProjectId())
	case "container":
		return u.Can(auth.PermAlerts) && x.project(o.GetProjectId())
	}
	return false
}

// canSeeRules 判断成员能不能打开规则页：任何一种规则能管就行。
func canSeeRules(u auth.User) bool {
	return u.Can(auth.PermSettings) || u.Can(auth.PermNodeManage) || u.Can(auth.PermAlerts) || u.Can(auth.PermProjectManage)
}

func (s *alertService) GetAlertRules(ctx context.Context, _ *connect.Request[nagiv1.GetAlertRulesRequest]) (*connect.Response[nagiv1.GetAlertRulesResponse], error) {
	u := auth.CurrentUser(ctx)
	if !canSeeRules(u) {
		return nil, permissionDenied()
	}
	x, err := s.loadAccess(ctx, u)
	if err != nil {
		return nil, err
	}
	policy, err := s.Nodes.AlertPolicy(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	all, err := s.Nodes.AlertOverrides(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &nagiv1.GetAlertRulesResponse{Builtin: &nagiv1.AlertPolicy{Rules: alert.Builtin(), Language: "zh-CN",
		FlapWindowMinutes: alert.DefaultFlapWindowMinutes, FlapCount: alert.DefaultFlapCount}, Policy: policy, CanEditPolicy: u.Can(auth.PermSettings)}
	for _, o := range all {
		if canEditOverride(u, x, o) {
			out.Overrides = append(out.Overrides, o)
		}
	}
	return connect.NewResponse(out), nil
}

func (s *alertService) UpdateAlertPolicy(ctx context.Context, req *connect.Request[nagiv1.UpdateAlertPolicyRequest]) (*connect.Response[nagiv1.AlertPolicy], error) {
	if !auth.CurrentUser(ctx).Can(auth.PermSettings) {
		return nil, permissionDenied()
	}
	p, err := s.Nodes.SetAlertPolicy(ctx, req.Msg.GetPolicy(), req.Msg.GetBaseRevision())
	if err != nil {
		return nil, alertRuleError(err)
	}
	s.Projects.PushAlertRules(context.WithoutCancel(ctx), "")
	return connect.NewResponse(p), nil
}

func (s *alertService) SetAlertOverride(ctx context.Context, req *connect.Request[nagiv1.SetAlertOverrideRequest]) (*connect.Response[nagiv1.AlertOverride], error) {
	u := auth.CurrentUser(ctx)
	x, err := s.loadAccess(ctx, u)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	o := &nagiv1.AlertOverride{Scope: m.GetScope(), NodeId: m.GetNodeId(), ProjectId: m.GetProjectId(), Container: m.GetContainer(), Rules: m.GetRules()}
	if !canEditOverride(u, x, o) {
		// 范围外的对象当作不存在，不暴露它是否存在；范围内但没有权限的明确拒绝。
		if o.GetScope() == "node" && !x.nodeInScope(o.GetNodeId()) {
			return nil, nodeNotFound()
		}
		if o.GetScope() != "node" && !x.project(o.GetProjectId()) {
			return nil, projectNotFound()
		}
		return nil, permissionDenied()
	}
	saved, err := s.Nodes.SetAlertOverride(ctx, o, m.GetBaseRevision())
	if err != nil {
		return nil, alertRuleError(err)
	}
	if saved.GetScope() != "node" {
		s.Projects.PushAlertRules(context.WithoutCancel(ctx), saved.GetProjectId())
	}
	return connect.NewResponse(saved), nil
}

func alertRuleError(err error) error {
	if errors.Is(err, nodes.ErrAlertConflict) {
		return connect.NewError(connect.CodeAborted, err)
	}
	var le *i18n.Error
	if errors.As(err, &le) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return toConnect(err)
}
