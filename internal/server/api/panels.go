package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/invite"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 换面板与多面板：新面板生成接入码，旧面板把它发给节点，节点用它加入新面板（见 docs/control-plane.md）。

const inviteTTL = time.Hour

func (s *settingsService) CreateInvite(ctx context.Context, _ *connect.Request[nagiv1.CreateInviteRequest]) (*connect.Response[nagiv1.CreateInviteResponse], error) {
	token, err := auth.RandomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	expires := now.Add(inviteTTL)
	if err := s.Q.CreateInvite(ctx, store.CreateInviteParams{
		TokenHash: auth.HashToken(token), CreatedAt: now.Unix(), ExpiresAt: expires.Unix(),
	}); err != nil {
		return nil, err
	}
	code := invite.Encode(invite.Invite{Address: s.Panel.PublicHostPort(ctx), Fingerprint: s.Panel.Fingerprint, Token: token})
	return connect.NewResponse(&nagiv1.CreateInviteResponse{Invite: code, ExpiresAt: timestamppb.New(expires)}), nil
}

func (s *settingsService) ListInvites(ctx context.Context, _ *connect.Request[nagiv1.ListInvitesRequest]) (*connect.Response[nagiv1.ListInvitesResponse], error) {
	rows, err := s.Q.ListInvites(ctx, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListInvitesResponse{}
	for _, r := range rows {
		out.Invites = append(out.Invites, &nagiv1.Invite{
			Id:        r.TokenHash[:12],
			CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0)),
			ExpiresAt: timestamppb.New(time.Unix(r.ExpiresAt, 0)),
			Uses:      uint32(r.Uses),
		})
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) RevokeInvite(ctx context.Context, req *connect.Request[nagiv1.RevokeInviteRequest]) (*connect.Response[nagiv1.RevokeInviteResponse], error) {
	n, err := s.Q.RevokeInvite(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("接入码不存在"))
	}
	return connect.NewResponse(&nagiv1.RevokeInviteResponse{}), nil
}

func (s *nodeService) agent(nodeID string) (nagiv1.AgentServiceClient, error) {
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return conn.Agent, nil
}

func (s *nodeService) ListNodePanels(ctx context.Context, req *connect.Request[nagiv1.ListNodePanelsRequest]) (*connect.Response[nagiv1.ListNodePanelsResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListPanels(ctx, &nagiv1.ListPanelsRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	for _, p := range resp.GetPanels() {
		p.IsSelf = p.GetFingerprint() == s.Panel.Fingerprint
	}
	return connect.NewResponse(&nagiv1.ListNodePanelsResponse{Panels: resp.GetPanels()}), nil
}

func (s *nodeService) AddNodePanel(ctx context.Context, req *connect.Request[nagiv1.AddNodePanelRequest]) (*connect.Response[nagiv1.AddNodePanelResponse], error) {
	inv, err := invite.Decode(req.Msg.GetInvite())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if inv.Fingerprint == s.Panel.Fingerprint {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("这是本面板自己的接入码，请到新面板上生成"))
	}
	nodeID := req.Msg.GetNodeId()
	agent, err := s.agent(nodeID)
	if err != nil {
		return nil, err
	}
	if _, err := agent.AddPanel(ctx, &nagiv1.AddPanelRequest{
		Address: inv.Address, Fingerprint: inv.Fingerprint, Token: inv.Token,
	}); err != nil {
		return nil, fromGRPC(err)
	}
	s.Log.Info("节点已加入另一个面板", "node", nodeID, "panel", inv.Address)
	if req.Msg.GetRemoveSelf() {
		if _, err := agent.RemovePanel(ctx, &nagiv1.RemovePanelRequest{Fingerprint: s.Panel.Fingerprint}); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("已加入新面板，但移除本面板失败：%w", fromGRPC(err)))
		}
		if err := s.forgetNode(ctx, nodeID); err != nil {
			return nil, err
		}
		s.Log.Info("节点已移交给另一个面板", "node", nodeID, "panel", inv.Address)
	}
	return connect.NewResponse(&nagiv1.AddNodePanelResponse{}), nil
}

func (s *nodeService) RemoveNodePanel(ctx context.Context, req *connect.Request[nagiv1.RemoveNodePanelRequest]) (*connect.Response[nagiv1.RemoveNodePanelResponse], error) {
	nodeID := req.Msg.GetNodeId()
	agent, err := s.agent(nodeID)
	if err != nil {
		return nil, err
	}
	if _, err := agent.RemovePanel(ctx, &nagiv1.RemovePanelRequest{Fingerprint: req.Msg.GetFingerprint()}); err != nil {
		return nil, fromGRPC(err)
	}
	if req.Msg.GetFingerprint() == s.Panel.Fingerprint {
		if err := s.forgetNode(ctx, nodeID); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&nagiv1.RemoveNodePanelResponse{}), nil
}
