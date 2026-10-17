package api

import (
	"bytes"
	"context"
	"errors"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 顶替：面板负责把设置下发给 A、B 和见证人（顺序是见证人 → B → A，A 收到后才开始续约），
// 之后的租约、顶替都由节点之间经 mesh 完成，面板只是见证人之一。

func (s *projectService) SetNodeProjectFailover(ctx context.Context, req *connect.Request[nagiv1.SetNodeProjectFailoverRequest]) (*connect.Response[nagiv1.FailoverStatus], error) {
	aID, id := req.Msg.GetNodeId(), req.Msg.GetProjectId()
	a, err := s.agent(aID)
	if err != nil {
		return nil, err
	}
	cur, err := a.GetFailover(ctx, &nagiv1.ReplicaRef{ProjectId: id})
	if err != nil {
		return nil, fromGRPC(err)
	}
	if req.Msg.GetStandbyNodeId() == "" {
		s.clearFailover(ctx, id, cur.GetConfig(), aID)
		return connect.NewResponse(&nagiv1.FailoverStatus{}), nil
	}
	if cur.GetConfig() != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("已经设置了顶替，请先取消再重新设置"))
	}

	bID, wID := req.Msg.GetStandbyNodeId(), req.Msg.GetWitnessNodeId()
	if bID == aID || wID == aID || (wID != "" && wID == bID) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("项目所在节点、顶替节点和见证人必须是三个不同的成员"))
	}
	b, err := s.agent(bID)
	if err != nil {
		return nil, err
	}
	member := func(nodeID, dir string) (*nagiv1.FailoverMember, error) {
		k, err := s.Nodes.NodeMeshKey(nodeID)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return &nagiv1.FailoverMember{Key: k[:], NodeId: nodeID, Name: s.nodeName(ctx, nodeID), Dir: dir}, nil
	}
	proj, err := a.GetProject(ctx, &nagiv1.GetProjectRequest{Id: id})
	if err != nil {
		return nil, fromGRPC(err)
	}
	// 设置时就检查 B：目录可用、B 上还没有这个项目。
	if _, err := b.CheckRestore(ctx, &nagiv1.CheckRestoreRequest{ProjectId: id, Dir: req.Msg.GetStandbyDir(),
		Routes: proj.GetProject().GetProject().GetRoutes()}); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("顶替节点检查不通过："+connectMessage(fromGRPC(err))))
	}
	cfg := &nagiv1.FailoverConfig{
		ProjectId: id, ProjectName: proj.GetProject().GetProject().GetName(),
		LeaseSeconds: req.Msg.GetLeaseSeconds(), AutoStateful: req.Msg.GetAutoStateful(),
	}
	if cfg.A, err = member(aID, proj.GetProject().GetProject().GetDir()); err != nil {
		return nil, err
	}
	if cfg.B, err = member(bID, req.Msg.GetStandbyDir()); err != nil {
		return nil, err
	}
	var w nagiv1.AgentServiceClient
	if wID == "" {
		m := s.Nodes.Mesh()
		if m == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("面板的 mesh 没有启动，不能担任见证人"))
		}
		k := m.PublicKey()
		cfg.Witness, cfg.WitnessIsPanel = &nagiv1.FailoverMember{Key: k[:], Name: "面板"}, true
	} else {
		if w, err = s.agent(wID); err != nil {
			return nil, err
		}
		if cfg.Witness, err = member(wID, ""); err != nil {
			return nil, err
		}
	}

	// 见证人 → B → A。任何一方失败就撤销已经下发的设置。
	if w == nil {
		err = s.Nodes.SetWitness(cfg)
	} else {
		_, err = w.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: cfg})
	}
	if err == nil {
		_, err = b.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: cfg})
	}
	var st *nagiv1.FailoverStatus
	if err == nil {
		st, err = a.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: cfg})
	}
	if err != nil {
		s.clearFailover(ctx, id, cfg, aID)
		return nil, fromGRPC(err)
	}
	s.Log.Info("已设置顶替", "project", id, "a", aID, "b", bID, "witness", cfg.GetWitness().GetName())
	return connect.NewResponse(st), nil
}

// clearFailover 通知所有参与者删除顶替设置（不在线的参与者以后再清理）。
func (s *projectService) clearFailover(ctx context.Context, id string, cfg *nagiv1.FailoverConfig, fallback string) {
	nodes := []string{fallback}
	if cfg != nil {
		nodes = []string{cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId(), cfg.GetWitness().GetNodeId()}
		if cfg.GetWitnessIsPanel() {
			s.Nodes.ClearWitness(id)
		}
	}
	for _, n := range nodes {
		if n == "" {
			continue
		}
		if c, err := s.agent(n); err == nil {
			_, _ = c.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: id})
		}
	}
}

func (s *projectService) GetNodeProjectFailover(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.FailoverStatus], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	st, err := a.GetFailover(ctx, &nagiv1.ReplicaRef{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}

func (s *nodeService) ListNodeFailovers(ctx context.Context, req *connect.Request[nagiv1.ListNodeReplicasRequest]) (*connect.Response[nagiv1.ListFailoversResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := a.ListFailovers(ctx, &nagiv1.ListFailoversRequest{})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *nodeService) ConfirmNodeTakeover(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.FailoverStatus], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	st, err := a.ConfirmTakeover(ctx, &nagiv1.ReplicaRef{ProjectId: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}

// ChangeNodeProjectWitness 更换见证人，顶替照常进行。新见证人从当前的持有者和轮次开始投票（不会把票投给别人），
// 然后按 B → A 的顺序更新设置，最后清掉旧见证人。任何一步失败都撤销已经做的。
// 旧见证人是另一个面板时这里清不掉它的设置，但 A、B 不会再找它投票，留下的设置不影响。
func (s *projectService) ChangeNodeProjectWitness(ctx context.Context, req *connect.Request[nagiv1.ChangeNodeProjectWitnessRequest]) (*connect.Response[nagiv1.FailoverStatus], error) {
	id := req.Msg.GetProjectId()
	n, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	cur, err := n.GetFailover(ctx, &nagiv1.ReplicaRef{ProjectId: id})
	if err != nil {
		return nil, fromGRPC(err)
	}
	cfg := cur.GetConfig()
	if cfg == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("项目没有设置顶替"))
	}
	aID, bID := cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId()
	a, err := s.agent(aID)
	if err != nil {
		return nil, err
	}
	b, err := s.agent(bID)
	if err != nil {
		return nil, err
	}
	// 当前的持有者和轮次：A、B 里轮次大的那个说了算。
	var holder []byte
	var epoch uint64
	for _, c := range []nagiv1.AgentServiceClient{a, b} {
		st, err := c.GetFailover(ctx, &nagiv1.ReplicaRef{ProjectId: id})
		if err != nil {
			return nil, fromGRPC(err)
		}
		if st.GetEpoch() > epoch && len(st.GetHolderKey()) == 32 {
			holder, epoch = st.GetHolderKey(), st.GetEpoch()
		}
	}
	if holder == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("读不到当前的持有者"))
	}

	next := proto.Clone(cfg).(*nagiv1.FailoverConfig)
	wID := req.Msg.GetWitnessNodeId()
	var w nagiv1.AgentServiceClient
	if wID == "" {
		m := s.Nodes.Mesh()
		if m == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("面板的 mesh 没有启动，不能担任见证人"))
		}
		k := m.PublicKey()
		if cfg.GetWitnessIsPanel() && bytes.Equal(cfg.GetWitness().GetKey(), k[:]) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("本面板已经是见证人"))
		}
		next.Witness, next.WitnessIsPanel = &nagiv1.FailoverMember{Key: k[:], Name: "面板"}, true
	} else {
		if wID == aID || wID == bID {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("见证人不能是项目所在的节点或顶替节点"))
		}
		if !cfg.GetWitnessIsPanel() && cfg.GetWitness().GetNodeId() == wID {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("这个节点已经是见证人"))
		}
		if w, err = s.agent(wID); err != nil {
			return nil, err
		}
		k, err := s.Nodes.NodeMeshKey(wID)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		next.Witness, next.WitnessIsPanel = &nagiv1.FailoverMember{Key: k[:], NodeId: wID, Name: s.nodeName(ctx, wID)}, false
	}

	clearNew := func() {
		if w == nil {
			s.Nodes.ClearWitness(id)
		} else {
			_, _ = w.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: id})
		}
	}
	if w == nil {
		err = s.Nodes.SetWitnessFrom(next, holder, epoch)
	} else {
		_, err = w.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next, SeedHolder: holder, SeedEpoch: epoch})
	}
	if err != nil {
		clearNew()
		return nil, fromGRPC(err)
	}
	if _, err := b.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next}); err != nil {
		clearNew()
		return nil, fromGRPC(err)
	}
	if _, err := a.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next}); err != nil {
		_, _ = b.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: cfg})
		clearNew()
		return nil, fromGRPC(err)
	}
	// 清掉旧见证人。
	if cfg.GetWitnessIsPanel() {
		if m := s.Nodes.Mesh(); m != nil {
			if k := m.PublicKey(); bytes.Equal(cfg.GetWitness().GetKey(), k[:]) {
				s.Nodes.ClearWitness(id)
			}
		}
	} else if c, err := s.agent(cfg.GetWitness().GetNodeId()); err == nil {
		_, _ = c.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: id})
	}
	s.Log.Info("已更换见证人", "project", id, "from", cfg.GetWitness().GetName(), "to", next.GetWitness().GetName())
	st, err := a.GetFailover(ctx, &nagiv1.ReplicaRef{ProjectId: id})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(st), nil
}
