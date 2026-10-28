package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/invite"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
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
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("接入码不存在"))
	}
	return connect.NewResponse(&nagiv1.RevokeInviteResponse{}), nil
}

// handoverProjects 返回移交这些节点时要一起移交的项目：和其中任何一个节点有关的项目。
// 项目有关的节点（容器所在、顶替、备份、见证）有不在里面的，返回错误并列出缺了哪些节点。
func (s *nodeService) handoverProjects(ctx context.Context, nodeIDs []string) ([]string, error) {
	selected := map[string]bool{}
	for _, id := range nodeIDs {
		selected[id] = true
	}
	projects, err := s.Q.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	var problems problemList
	for _, p := range projects {
		related, err := s.Projects.RelatedNodes(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		touched := false
		var missing i18n.List
		for n := range related {
			if selected[n] {
				touched = true
			} else {
				missing = append(missing, s.Projects.NodeName(ctx, n))
			}
		}
		if !touched {
			continue
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			problems = append(problems, i18n.Errorf("项目 %s 还需要一起移交节点 %s", p.Name, missing))
			continue
		}
		out = append(out, p.ID)
	}
	if len(problems) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("移交按整个项目进行：%s", problems))
	}
	return out, nil
}

// problemList 是几条说明，作为 i18n 的参数时按语言用分号连起来。
type problemList []error

func (p problemList) String() string { return p.Localize(i18n.Zh) }

func (p problemList) Localize(l i18n.Lang) string {
	texts := make([]string, len(p))
	for i, e := range p {
		texts[i] = i18n.Localize(l, e)
	}
	return strings.Join(texts, i18n.Sprintf(l, "；"))
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

func (s *nodeService) HandoverNodes(ctx context.Context, req *connect.Request[nagiv1.HandoverNodesRequest]) (*connect.Response[nagiv1.HandoverNodesResponse], error) {
	inv, err := invite.Decode(req.Msg.GetInvite())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if inv.Fingerprint == s.Panel.Fingerprint {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("这是本面板自己的接入码，请到新面板上生成"))
	}
	ids := req.Msg.GetNodeIds()
	if len(ids) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("请选择要移交的节点"))
	}
	for _, id := range ids {
		if _, err := s.agent(id); err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, i18n.Errorf("节点 %s 不在线", s.Projects.NodeName(ctx, id)))
		}
	}
	// 移交按整个项目：项目相关的节点必须一起移交，免得项目被拆到两个面板上。
	moving, err := s.handoverProjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	out := &nagiv1.HandoverNodesResponse{}
	failed := 0
	for _, id := range ids {
		res := &nagiv1.HandoverResult{NodeId: id}
		out.Results = append(out.Results, res)
		agent, err := s.agent(id)
		if err == nil {
			_, err = agent.AddPanel(ctx, &nagiv1.AddPanelRequest{Address: inv.Address, Fingerprint: inv.Fingerprint, Token: inv.Token})
		}
		if err == nil {
			s.Log.Info("节点已加入另一个面板", "node", id, "panel", inv.Address)
			if _, rerr := agent.RemovePanel(ctx, &nagiv1.RemovePanelRequest{Fingerprint: s.Panel.Fingerprint}); rerr != nil {
				err = i18n.Errorf("已加入新面板，但移除本面板失败：%w", rerr)
			}
		}
		if err != nil {
			if isStatus(err) {
				res.Error = status.Convert(err).Message()
			} else {
				res.Error = i18n.Localize(i18n.FromContext(ctx), err)
			}
			failed++
		}
	}
	if failed > 0 {
		// 有节点没移交成功：项目留在本面板上，已经移交的节点照常（新面板拿到的缓存是一样的）。
		return connect.NewResponse(out), nil
	}
	// 新面板从节点上缓存的配置拼回这些项目，本面板忘记它们和这些节点。
	for _, id := range moving {
		s.Projects.ForgetHandedOver(ctx, id)
		_ = s.Q.DeleteProject(ctx, id)
	}
	for _, id := range ids {
		if err := s.forgetNode(ctx, id); err != nil {
			return nil, err
		}
	}
	s.Log.Info("节点已移交给另一个面板", "nodes", len(ids), "panel", inv.Address, "projects", len(moving))
	return connect.NewResponse(out), nil
}

func (s *nodeService) RemoveNodePanel(ctx context.Context, req *connect.Request[nagiv1.RemoveNodePanelRequest]) (*connect.Response[nagiv1.RemoveNodePanelResponse], error) {
	nodeID := req.Msg.GetNodeId()
	// 本面板的备用面板由主面板的成员列表管，从单个节点上移除它马上又会被加回来：要在设置的「备用面板」里移除。
	if _, err := s.Q.GetStandbyPanelByServerFP(ctx, req.Msg.GetFingerprint()); err == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("这是本面板的备用面板，请在设置的「备用面板」里移除，所有节点会一起移除它"))
	}
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
		return nil, connect.NewError(connect.CodeUnavailable, i18n.New("面板的 mesh 没有启动"))
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
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("只能升级 Linux 节点"))
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

// ---- 副本 ----

func (s *nodeService) ListNodeReplicas(ctx context.Context, req *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.ListNodeReplicasResponse], error) {
	nodeID := req.Msg.GetNodeId()
	agent, err := s.agent(nodeID)
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListReplicas(ctx, &nagiv1.ListReplicasRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	out := &nagiv1.ListNodeReplicasResponse{}
	for _, r := range resp.GetReplicas() {
		out.Replicas = append(out.Replicas, &nagiv1.NodeReplica{Replica: r, InUse: s.Projects.ReplicaInUse(ctx, nodeID, r.GetProjectId(), r.GetContainer())})
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) DeleteNodeReplica(ctx context.Context, req *connect.Request[nagiv1.DeleteNodeReplicaRequest]) (*connect.Response[nagiv1.DeleteNodeReplicaResponse], error) {
	m := req.Msg
	if s.Projects.ReplicaInUse(ctx, m.GetNodeId(), m.GetProjectId(), m.GetContainer()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("这个副本还在使用：先在容器上取消备份节点或顶替"))
	}
	agent, err := s.agent(m.GetNodeId())
	if err != nil {
		return nil, err
	}
	if _, err := agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: m.GetProjectId(), Container: m.GetContainer()}); err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.DeleteNodeReplicaResponse{}), nil
}
