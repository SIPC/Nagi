package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/invite"
	"github.com/SIPC/nagi/internal/pkg/mesh"
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

// handoverProjects 返回节点上的容器所属的项目；有项目还有容器在别的节点上时返回错误。
func (s *nodeService) handoverProjects(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := s.Q.ListNodeContainers(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range rows {
		if seen[c.ProjectID] {
			continue
		}
		seen[c.ProjectID] = true
		all, err := s.Q.ListContainers(ctx, c.ProjectID)
		if err != nil {
			return nil, err
		}
		for _, o := range all {
			if o.NodeID != nodeID {
				name := c.ProjectID
				if p, err := s.Q.GetProject(ctx, c.ProjectID); err == nil {
					name = p.Name
				}
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"项目 %s 还有容器在别的节点上，移交要按整个项目进行：先把它的容器迁移到同一个节点，或者把相关节点一起移交", name))
			}
		}
		out = append(out, c.ProjectID)
	}
	return out, nil
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
	// 移交按整个项目：项目还有容器在别的节点上时拒绝，免得项目被拆到两个面板上。
	moving, err := s.handoverProjects(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if _, err := agent.AddPanel(ctx, &nagiv1.AddPanelRequest{
		Address: inv.Address, Fingerprint: inv.Fingerprint, Token: inv.Token,
	}); err != nil {
		return nil, fromGRPC(err)
	}
	s.Log.Info("节点已加入另一个面板", "node", nodeID, "panel", inv.Address)
	if _, err := agent.RemovePanel(ctx, &nagiv1.RemovePanelRequest{Fingerprint: s.Panel.Fingerprint}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("已加入新面板，但移除本面板失败：%w", fromGRPC(err)))
	}
	// 新面板从节点上缓存的配置拼回这些项目，本面板忘记它们。
	for _, id := range moving {
		_ = s.Q.DeleteProject(ctx, id)
	}
	if err := s.forgetNode(ctx, nodeID); err != nil {
		return nil, err
	}
	s.Log.Info("节点已移交给另一个面板", "node", nodeID, "panel", inv.Address, "projects", len(moving))
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

// ---- mesh ----

func (s *nodeService) GetNodeMesh(ctx context.Context, req *connect.Request[nagiv1.GetNodeMeshRequest]) (*connect.Response[nagiv1.MeshStatus], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.GetMeshStatus(ctx, &nagiv1.MeshStatusRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) PingNodeMesh(ctx context.Context, req *connect.Request[nagiv1.PingNodeMeshRequest]) (*connect.Response[nagiv1.MeshPingResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.MeshPing(ctx, &nagiv1.MeshPingRequest{PublicKey: req.Msg.GetPublicKey()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) GetPanelMesh(context.Context, *connect.Request[nagiv1.GetPanelMeshRequest]) (*connect.Response[nagiv1.MeshStatus], error) {
	m := s.Nodes.Mesh()
	if m == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("面板的 mesh 没有启动"))
	}
	return connect.NewResponse(mesh.StatusProto(m, s.Nodes.MeshPeerName)), nil
}

// ---- 升级节点 ----

func (s *nodeService) UpgradeNode(ctx context.Context, req *connect.Request[nagiv1.UpgradeNodeRequest]) (*connect.Response[nagiv1.UpgradeResponse], error) {
	conn, err := s.Nodes.Get(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	info := conn.LastReport().GetNode()
	if info.GetOs() != "linux" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("只能升级 Linux 节点"))
	}
	path, err := s.binaryPath(info.GetArch())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return nil, err
	}
	resp, err := conn.Agent.Upgrade(ctx, &nagiv1.UpgradeRequest{Sha256: hex.EncodeToString(h.Sum(nil))})
	if err != nil {
		return nil, fromGRPC(err)
	}
	s.Log.Info("节点正在升级", "node", req.Msg.GetNodeId(), "from", info.GetVersion(), "to", resp.GetVersion())
	return connect.NewResponse(resp), nil
}

// ---- 镜像加速、项目根目录 ----

func (s *nodeService) GetNodeMirrors(ctx context.Context, req *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.Mirrors], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := a.GetMirrors(ctx, &nagiv1.GetMirrorsRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) SetNodeMirrors(ctx context.Context, req *connect.Request[nagiv1.SetNodeMirrorsRequest]) (*connect.Response[nagiv1.Mirrors], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := a.SetMirrors(ctx, &nagiv1.Mirrors{Mirrors: req.Msg.GetMirrors()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) SetNodeProjectsRoot(ctx context.Context, req *connect.Request[nagiv1.SetNodeProjectsRootRequest]) (*connect.Response[nagiv1.SetProjectsRootResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := a.SetProjectsRoot(ctx, &nagiv1.SetProjectsRootRequest{Root: req.Msg.GetRoot()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}
