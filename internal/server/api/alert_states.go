package api

import (
	"context"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/proto"
)

type alertService struct{ *API }

func (s *alertService) cleanAlert(ctx context.Context, a *nagiv1.AlertState) (*nagiv1.AlertState, error) {
	x, err := s.loadAccess(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return nil, err
	}
	v := proto.CloneOf(a)
	if v.Alert.GetRule() == "" {
		if n, ok := alert.NoticeOf(v.Alert.GetKey()); ok {
			title := n.Zh
			if i18n.FromContext(ctx) == i18n.En {
				title = n.En
			}
			// Localize known event kinds without losing the original event details.
			if v.Alert.Title != "" && v.Alert.Title != title {
				v.Alert.Message = v.Alert.Title + "\n" + v.Alert.Message
			}
			v.Alert.Title = title
		}
	}
	if x != nil {
		if !x.node(v.NodeId) {
			v.NodeId = ""
		}
		v.Alert.Node = ""
		v.Alert.Title = s.scrubNodes(ctx, x, v.Alert.Title)
		v.Alert.Message = s.scrubNodes(ctx, x, v.Alert.Message)
		v.Conclusion = s.scrubNodes(ctx, x, v.Conclusion)
		if !x.can(auth.PermAddresses) {
			v.Alert.Title, v.Alert.Message, v.Conclusion = scrubIPs(v.Alert.Title), scrubIPs(v.Alert.Message), scrubIPs(v.Conclusion)
		}
	}
	return v, nil
}

// Audit access does not override project scope for live alerts. In particular a
// shared node must not reveal another project's messages or existence.
func (s *alertService) visibleAlert(ctx context.Context, a *nagiv1.AlertState) (bool, error) {
	x, err := s.loadAccess(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return false, err
	}
	if x == nil {
		return true, nil
	}
	if a.Alert.GetProject() != "" {
		return x.project(a.Alert.Project) && x.can(auth.PermView), nil
	}
	return x.projects == nil && (a.NodeId == "" || x.node(a.NodeId)), nil
}

func (s *alertService) ListAlertStates(ctx context.Context, req *connect.Request[nagiv1.ListAlertStatesRequest]) (*connect.Response[nagiv1.ListAlertStatesResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s.Nodes == nil {
		return connect.NewResponse(&nagiv1.ListAlertStatesResponse{}), nil
	}
	recent := false
	if s.Nodes.ReadOnly {
		ids := []string{req.Msg.NodeId}
		if req.Msg.NodeId == "" {
			nodes, err := s.Q.ListNodes(ctx)
			if err != nil {
				return nil, err
			}
			for _, n := range nodes {
				ids = append(ids, n.ID)
			}
		}
		// Reserve time to read the saved copy when the primary cannot be reached.
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
		for _, id := range ids {
			if err := s.Nodes.RefreshAlertReplica(refreshCtx, id); err != nil {
				recent = true
				break
			}
		}
		refreshCancel()
	}
	states, err := s.Nodes.AlertStates(ctx, req.Msg.NodeId, req.Msg.IncludeHistory)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &nagiv1.ListAlertStatesResponse{RecentCopy: recent}
	for _, state := range states {
		ok, err := s.visibleAlert(ctx, state)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		v, err := s.cleanAlert(ctx, state)
		if err != nil {
			return nil, err
		}
		out.Alerts = append(out.Alerts, v)
	}
	return connect.NewResponse(out), nil
}

func (s *alertService) HandleAlert(ctx context.Context, req *connect.Request[nagiv1.HandleAlertRequest]) (*connect.Response[nagiv1.HandleAlertResponse], error) {
	u := auth.CurrentUser(ctx)
	if !u.Can(auth.PermAudit) {
		return nil, permissionDenied()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	states, err := s.Nodes.AlertStates(ctx, req.Msg.NodeId, true)
	if err != nil {
		return nil, toConnect(err)
	}
	var target *nagiv1.AlertState
	for _, a := range states {
		if (req.Msg.NodeId == "" || a.NodeId == req.Msg.NodeId) && a.Alert.GetKey() == req.Msg.Key && a.Alert.GetOccurrence() == req.Msg.Occurrence {
			target = a
			break
		}
	}
	if target == nil {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("告警不存在或已超出历史保留范围"))
	}
	ok, err := s.visibleAlert(ctx, target)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("告警不存在或已超出历史保留范围"))
	}
	req.Msg = proto.CloneOf(req.Msg)
	req.Msg.NodeId = target.NodeId
	actor := strconv.FormatInt(u.ID, 10)
	if s.Nodes.ReadOnly {
		if s.Nodes.AlertPrimary == nil {
			return nil, connect.NewError(connect.CodeUnavailable, i18n.New("告警处理需要连接当前主面板"))
		}
		cc, err := s.Nodes.AlertPrimary(ctx)
		if err != nil {
			return nil, toConnect(err)
		}
		defer cc.Close()
		v, err := nagiv1.NewPanelAlertServiceClient(cc).RelayHandleAlert(ctx, &nagiv1.RelayHandleAlertRequest{Request: req.Msg, Actor: s.Nodes.Self + ":" + actor})
		if err != nil {
			return nil, toConnect(err)
		}
		if err := s.Nodes.ImportAlertReplica(ctx, v); err != nil {
			return nil, toConnect(err)
		}
		list, err := s.Nodes.AlertStates(ctx, req.Msg.NodeId, true)
		if err != nil {
			return nil, toConnect(err)
		}
		for _, a := range list {
			if a.Alert.Key == req.Msg.Key && a.Alert.Occurrence == req.Msg.Occurrence {
				v, err := s.cleanAlert(ctx, a)
				if err != nil {
					return nil, err
				}
				return connect.NewResponse(&nagiv1.HandleAlertResponse{Alert: v}), nil
			}
		}
		return nil, connect.NewError(connect.CodeAborted, i18n.New("处理状态已改变，请刷新"))
	}
	a, err := s.Nodes.HandleAlert(ctx, req.Msg, actor)
	if err != nil {
		return nil, toConnect(err)
	}
	v, err := s.cleanAlert(ctx, a)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.HandleAlertResponse{Alert: v}), nil
}
