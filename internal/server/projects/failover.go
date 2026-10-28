package projects

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 顶替（见 docs/failover.md#节点故障指定顶替节点），按容器：面板把设置下发给见证人、B、A（A 收到后才开始续约），
// 之后的租约、顶替、切回都由节点之间经 mesh 完成，面板只是见证人之一。持有者上报更高的轮次时，
// 面板把容器的所在节点改成它（Sync）。

func failoverOf(c store.Container) *nagiv1.FailoverConfig {
	if len(c.Failover) == 0 {
		return nil
	}
	cfg := &nagiv1.FailoverConfig{}
	if proto.Unmarshal(c.Failover, cfg) != nil {
		return nil
	}
	return cfg
}

// standbyOf 返回顶替设置里不是容器当前所在节点的那一方（热备）。
func standbyOf(cfg *nagiv1.FailoverConfig, holderNode string) *nagiv1.FailoverParticipant {
	if cfg.GetA().GetNodeId() == holderNode {
		return cfg.GetB()
	}
	return cfg.GetA()
}

func isParticipant(cfg *nagiv1.FailoverConfig, nodeID string) bool {
	return cfg.GetA().GetNodeId() == nodeID || cfg.GetB().GetNodeId() == nodeID
}

func noFailover(c store.Container) error {
	if len(c.Failover) > 0 {
		return invalid("容器 %s 设置了顶替，请先取消顶替", c.Name)
	}
	return nil
}

func (s *Service) participant(ctx context.Context, nodeID, dir string) (*nagiv1.FailoverParticipant, error) {
	n, err := s.Q.GetNode(ctx, nodeID)
	if err != nil {
		return nil, notFoundError{i18n.New("节点不存在")}
	}
	if len(n.MeshKey) != 32 {
		return nil, invalid("节点 %s 还没有加入 mesh", n.Name)
	}
	return &nagiv1.FailoverParticipant{Key: n.MeshKey, NodeId: n.ID, Name: n.Name, Dir: dir}, nil
}

// witnessFor 返回见证人：nodeID 为空时是面板。
func (s *Service) witnessFor(ctx context.Context, nodeID string) (*nagiv1.FailoverParticipant, nagiv1.AgentServiceClient, error) {
	if nodeID == "" {
		m := s.Nodes.Mesh()
		if m == nil {
			return nil, nil, invalid("面板的 mesh 没有启动，不能担任见证人")
		}
		k := m.PublicKey()
		return &nagiv1.FailoverParticipant{Key: k[:], Name: "面板"}, nil, nil
	}
	agent, err := s.agent(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.participant(ctx, nodeID, "")
	return p, agent, err
}

// SetFailoverRequest 是顶替设置。
type SetFailoverRequest struct {
	ProjectID, Container, StandbyNodeID, StandbyDir, WitnessNodeID string
	LeaseSeconds                                                   uint32
	AutoStateful, AutoFailback                                     bool
}

// SetFailover 设置顶替：检查顶替节点（目录、端口），在它上面登记副本，再按见证人 → B → A 的顺序下发设置，
// 任何一方失败就撤销已经下发的。之后 A 把快照同步给 B，B 提前拉好镜像。
func (s *Service) SetFailover(ctx context.Context, req SetFailoverRequest) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(req.ProjectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, req.ProjectID)
	if err != nil {
		return nil, err
	}
	c, err := s.container(ctx, req.ProjectID, req.Container)
	if err != nil {
		return nil, err
	}
	if err := noFailover(c); err != nil {
		return nil, invalid("容器 %s 已经设置了顶替，请先取消再重新设置", c.Name)
	}
	if c.BackupNodeID != "" {
		return nil, invalid("容器 %s 设置了备份节点，请先取消备份节点再设置顶替（顶替节点本身就有一份随时更新的副本）", c.Name)
	}
	aID, bID, wID := c.NodeID, req.StandbyNodeID, req.WitnessNodeID
	if bID == "" {
		return nil, invalid("请选择顶替节点")
	}
	if bID == aID || wID == aID || (wID != "" && wID == bID) {
		return nil, invalid("容器所在节点、顶替节点和见证人必须是三个不同的成员")
	}
	if req.LeaseSeconds != 0 && (req.LeaseSeconds < 9 || req.LeaseSeconds > 600) {
		return nil, invalid("租约时长须在 9 到 600 秒之间")
	}
	a, err := s.agent(ctx, aID)
	if err != nil {
		return nil, err
	}
	b, bNode, err := s.targetNode(ctx, bID)
	if err != nil {
		return nil, err
	}
	dir, err := s.placeDir(ctx, b, p, c, req.StandbyDir)
	if err != nil {
		return nil, err
	}
	sp, err := s.checkPlacement(ctx, b, p, c, dir)
	if err != nil {
		return nil, err
	}
	cfg := &nagiv1.FailoverConfig{
		ProjectId: p.ID, ProjectName: p.Name, Container: c.Name,
		LeaseSeconds: req.LeaseSeconds, AutoStateful: req.AutoStateful, AutoFailback: req.AutoFailback,
	}
	if cfg.A, err = s.participant(ctx, aID, c.Dir); err != nil {
		return nil, err
	}
	if cfg.B, err = s.participant(ctx, bID, dir); err != nil {
		return nil, err
	}
	var w nagiv1.AgentServiceClient
	if cfg.Witness, w, err = s.witnessFor(ctx, wID); err != nil {
		return nil, err
	}
	cfg.WitnessIsPanel = w == nil
	aNode, err := s.Q.GetNode(ctx, aID)
	if err != nil {
		return nil, err
	}
	// B 登记副本（只接受 A 的同步），带上它恢复时用的配置。
	if err := s.acceptReplica(ctx, b, sp, aNode); err != nil {
		return nil, err
	}
	// 见证人 → B → A。
	set := &nagiv1.SetFailoverRequest{Config: cfg}
	if w == nil {
		err = s.Nodes.SetWitness(cfg)
	} else {
		_, err = w.SetFailover(ctx, set)
	}
	if err == nil {
		_, err = b.Agent.SetFailover(ctx, set)
	}
	if err == nil {
		_, err = a.SetFailover(ctx, set)
	}
	if err != nil {
		s.clearFailover(ctx, cfg)
		_, _ = b.Agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: p.ID, Container: c.Name})
		return nil, err
	}
	raw, _ := proto.Marshal(cfg)
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: raw, FailoverEpoch: 1, ProjectID: p.ID, Name: c.Name}); err != nil {
		return nil, err
	}
	s.refreshLinks()
	// 立即开始第一次同步，热备才能顶替。
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if _, err := a.CreateSnapshot(sctx, &nagiv1.ContainerRef{ProjectId: p.ID, Container: c.Name}); err != nil {
			s.Log.Warn("设置顶替后的第一次快照失败", "project", p.ID, "container", c.Name, "err", err)
		}
	}()
	s.Log.Info("已设置顶替", "project", p.ID, "container", c.Name, "a", aID, "b", bNode.Name, "witness", cfg.GetWitness().GetName())
	return s.reload(ctx, p.ID)
}

// clearFailover 通知所有参与者删除顶替设置（不在线的参与者以后由 Sync 清理）。
func (s *Service) clearFailover(ctx context.Context, cfg *nagiv1.FailoverConfig) {
	if cfg.GetWitnessIsPanel() {
		s.Nodes.ClearWitness(cfg.GetProjectId(), cfg.GetContainer())
	}
	for _, n := range []string{cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId(), cfg.GetWitness().GetNodeId()} {
		if n == "" {
			continue
		}
		if agent, err := s.agent(ctx, n); err == nil {
			_, _ = agent.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: cfg.GetProjectId(), Container: cfg.GetContainer()})
		}
	}
}

// ClearFailover 取消顶替：所有参与者删除设置，热备上的副本删除。容器留在当前所在的节点上。
// 要求容器所在的节点在线（否则热备可能正要顶替）。
func (s *Service) ClearFailover(ctx context.Context, projectID, name string) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	cfg := failoverOf(c)
	if cfg == nil {
		return s.reload(ctx, projectID)
	}
	if _, err := s.agent(ctx, c.NodeID); err != nil {
		return nil, err
	}
	if err := s.dropFailover(ctx, c, cfg); err != nil {
		return nil, err
	}
	s.Log.Info("已取消顶替", "project", projectID, "container", name)
	return s.reload(ctx, projectID)
}

// dropFailover 删除容器的顶替设置（各参与者、热备上的副本、面板上的记录）。调用方持有项目锁。
func (s *Service) dropFailover(ctx context.Context, c store.Container, cfg *nagiv1.FailoverConfig) error {
	s.clearFailover(ctx, cfg)
	if st := standbyOf(cfg, c.NodeID); st.GetNodeId() != "" {
		if agent, err := s.agent(ctx, st.GetNodeId()); err == nil {
			_, _ = agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: c.ProjectID, Container: c.Name})
		}
	}
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: nil, FailoverEpoch: 0, ProjectID: c.ProjectID, Name: c.Name}); err != nil {
		return err
	}
	s.refreshLinks()
	return nil
}

// ChangeWitness 更换见证人，顶替照常进行。新见证人从当前的持有者和轮次开始投票（不会把票投给别人），
// 然后按 B → A 的顺序更新设置，最后清掉旧见证人。任何一步失败都撤销已经做的。
func (s *Service) ChangeWitness(ctx context.Context, projectID, name, witnessNodeID string) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	cfg := failoverOf(c)
	if cfg == nil {
		return nil, invalid("容器 %s 没有设置顶替", name)
	}
	a, err := s.agent(ctx, cfg.GetA().GetNodeId())
	if err != nil {
		return nil, err
	}
	b, err := s.agent(ctx, cfg.GetB().GetNodeId())
	if err != nil {
		return nil, err
	}
	if witnessNodeID == cfg.GetA().GetNodeId() || witnessNodeID == cfg.GetB().GetNodeId() {
		return nil, invalid("见证人不能是容器所在的节点或顶替节点")
	}
	if witnessNodeID != "" && cfg.GetWitness().GetNodeId() == witnessNodeID {
		return nil, invalid("这个节点已经是见证人")
	}
	if m := s.Nodes.Mesh(); witnessNodeID == "" && cfg.GetWitnessIsPanel() && m != nil {
		if k := m.PublicKey(); string(cfg.GetWitness().GetKey()) == string(k[:]) {
			return nil, invalid("本面板已经是见证人")
		}
	}
	// 当前的持有者和轮次：A、B 上报的里轮次大的那个说了算。
	var holder []byte
	var epoch uint64
	for _, n := range []string{cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId()} {
		if st := s.reportedFailover(n, projectID, name); st != nil && st.GetEpoch() > epoch && len(st.GetHolderKey()) == 32 {
			holder, epoch = st.GetHolderKey(), st.GetEpoch()
		}
	}
	if holder == nil {
		return nil, invalid("读不到当前的持有者，请稍后再试")
	}
	next := proto.Clone(cfg).(*nagiv1.FailoverConfig)
	var w nagiv1.AgentServiceClient
	if next.Witness, w, err = s.witnessFor(ctx, witnessNodeID); err != nil {
		return nil, err
	}
	next.WitnessIsPanel = w == nil
	clearNew := func() {
		if w == nil {
			s.Nodes.ClearWitness(projectID, name)
		} else {
			_, _ = w.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: projectID, Container: name})
		}
	}
	if w == nil {
		err = s.Nodes.SetWitnessFrom(next, holder, epoch)
	} else {
		_, err = w.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next, SeedHolder: holder, SeedEpoch: epoch})
	}
	if err != nil {
		clearNew()
		return nil, err
	}
	if _, err := b.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next}); err != nil {
		clearNew()
		return nil, err
	}
	if _, err := a.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: next}); err != nil {
		_, _ = b.SetFailover(ctx, &nagiv1.SetFailoverRequest{Config: cfg})
		clearNew()
		return nil, err
	}
	// 清掉旧见证人。
	if cfg.GetWitnessIsPanel() {
		if !next.GetWitnessIsPanel() {
			s.Nodes.ClearWitness(projectID, name) // 旧见证人是另一个面板时，本面板没有这份设置，清了也没关系
		}
	} else if old, err := s.agent(ctx, cfg.GetWitness().GetNodeId()); err == nil {
		_, _ = old.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: projectID, Container: name})
	}
	raw, _ := proto.Marshal(next)
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: raw, FailoverEpoch: c.FailoverEpoch, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	from := cfg.GetWitness().GetName()
	if cfg.GetWitnessIsPanel() {
		from = "旧面板"
		if m := s.Nodes.Mesh(); m != nil {
			if self := m.PublicKey(); string(cfg.GetWitness().GetKey()) == string(self[:]) {
				from = "本面板"
			}
		}
	}
	to := next.GetWitness().GetName()
	if next.GetWitnessIsPanel() {
		to = "本面板"
	}
	s.Log.Info("已更换见证人", "project", projectID, "container", name, "from", from, "to", to)
	return s.reload(ctx, projectID)
}

// ConfirmTakeover 有状态容器的所在节点失联时，确认由热备顶替（接受丢失最后一次同步之后的数据）。
func (s *Service) ConfirmTakeover(ctx context.Context, projectID, name string) (*nagiv1.Project, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	cfg := failoverOf(c)
	if cfg == nil {
		return nil, invalid("容器 %s 没有设置顶替", name)
	}
	agent, err := s.agent(ctx, standbyOf(cfg, c.NodeID).GetNodeId())
	if err != nil {
		return nil, err
	}
	if _, err := agent.ConfirmTakeover(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name}); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// reportedFailover 返回节点最近一次上报的某个容器的顶替状态。
func (s *Service) reportedFailover(nodeID, projectID, name string) *nagiv1.FailoverStatus {
	conn, err := s.Nodes.Peek(nodeID)
	if err != nil {
		return nil
	}
	for _, st := range conn.LastReport().GetFailovers() {
		if st.GetConfig().GetProjectId() == projectID && st.GetConfig().GetContainer() == name {
			return st
		}
	}
	return nil
}

// syncFailovers 处理节点上报的顶替状态：
//   - 持有者上报了更高的轮次（顶替或切回了）：面板把容器的所在节点改成它；
//   - 热备上的配置旧了：重新登记副本，带上最新的配置；
//   - 面板上已经取消了的顶替：让节点也删掉。
func (s *Service) syncFailovers(ctx context.Context, conn *nodes.Conn, report []*nagiv1.FailoverStatus) {
	for _, st := range report {
		cfg := st.GetConfig()
		pid, name := cfg.GetProjectId(), cfg.GetContainer()
		// 在项目的锁里处理：设置、更换、取消顶替时先通知各节点再写数据库，中间收到的上报不能按旧记录处理
		// （例如热备刚收到设置、数据库还没写，就让它删掉）。项目正在操作时跳过，下次上报再说。
		l := s.lock(pid)
		if !l.TryLock() {
			continue
		}
		adopt := s.syncFailover(ctx, conn, st, pid, name)
		l.Unlock()
		if adopt != nil {
			s.adoptWitness(adopt.c, adopt.cfg)
		}
	}
}

type witnessAdopt struct {
	c   store.Container
	cfg *nagiv1.FailoverConfig
}

// syncFailover 处理一个容器的顶替状态。调用方持有项目的锁。需要把见证人换成本面板时返回它（在锁外做）。
func (s *Service) syncFailover(ctx context.Context, conn *nodes.Conn, st *nagiv1.FailoverStatus, pid, name string) *witnessAdopt {
	clear := func() {
		_, _ = conn.Agent.SetFailover(ctx, &nagiv1.SetFailoverRequest{Clear: true, ProjectId: pid, Container: name})
	}
	c, err := s.container(ctx, pid, name)
	if errors.Is(err, ErrNotFound) {
		if p, perr := s.project(ctx, pid); perr == nil && !recentlyRebuilt(p) {
			clear() // 容器已经删了：这个节点上的设置也删掉。
		}
		return nil
	}
	if err != nil {
		return nil
	}
	cur := failoverOf(c)
	if cur == nil {
		// 项目刚从节点缓存拼回时，容器可能还没从持有者那里拼回顶替设置（例如先从热备拼回的）：先不删节点上的。
		if p, perr := s.project(ctx, pid); perr == nil && recentlyRebuilt(p) {
			return nil
		}
		clear()
		return nil
	}
	if !isParticipant(cur, conn.NodeID) {
		// 节点既不是 A、B，也不是现在的见证人：它是以前的见证人（更换或取消时它离线，没收到），让它删掉。
		if cur.GetWitnessIsPanel() || cur.GetWitness().GetNodeId() != conn.NodeID {
			clear()
			s.Log.Info("节点已不是这个容器的见证人，删掉它上面的顶替设置", "project", pid, "container", name, "node", conn.NodeID)
		}
		return nil
	}
	switch {
	case st.GetRole() == "holder" && st.GetEpoch() > uint64(c.FailoverEpoch) && !s.witnessDisagrees(c, cur, conn.NodeID, st.GetEpoch()):
		dir := cur.GetA().GetDir()
		if cur.GetB().GetNodeId() == conn.NodeID {
			dir = cur.GetB().GetDir()
		}
		if c.NodeID != conn.NodeID || dir != c.Dir {
			s.Log.Warn("容器已由另一个节点运行（顶替或切回）", "project", pid, "container", name, "from", c.NodeID, "to", conn.NodeID, "epoch", st.GetEpoch())
		}
		if err := s.Q.SetContainerHolder(ctx, store.SetContainerHolderParams{NodeID: conn.NodeID, Dir: dir, FailoverEpoch: int64(st.GetEpoch()), ProjectID: pid, Name: name}); err == nil {
			s.refreshLinks()
		}
	case st.GetRole() == "standby" && c.NodeID != conn.NodeID && st.GetSpecRevision() > 0 &&
		(st.GetSpecRevision() != c.Revision || st.GetSpecProjectRevision() != s.projectRevision(ctx, pid)):
		s.refreshStandby(ctx, c, cur, conn)
	}
	return &witnessAdopt{c, cur}
}

func (s *Service) projectRevision(ctx context.Context, id string) int64 {
	p, err := s.project(ctx, id)
	if err != nil {
		return 0
	}
	return p.Revision
}

// refreshStandby 把容器最新的配置推给热备（重新登记副本，只接受当前持有者的同步）。
func (s *Service) refreshStandby(ctx context.Context, c store.Container, cfg *nagiv1.FailoverConfig, standby *nodes.Conn) {
	p, err := s.project(ctx, c.ProjectID)
	if err != nil {
		return
	}
	sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
	if err != nil {
		return
	}
	sp.Dir = standbyOf(cfg, c.NodeID).GetDir()
	holder, err := s.Q.GetNode(ctx, c.NodeID)
	if err != nil {
		return
	}
	if err := s.acceptReplica(ctx, standby, sp, holder); err != nil {
		s.Log.Warn("把最新的配置推给热备失败", "project", c.ProjectID, "container", c.Name, "node", standby.NodeID, "err", err)
	}
}

// pushStandby 容器的配置变了以后推给热备（在线时）。
func (s *Service) pushStandby(ctx context.Context, c store.Container) {
	cfg := failoverOf(c)
	if cfg == nil {
		return
	}
	conn, err := s.Nodes.Get(standbyOf(cfg, c.NodeID).GetNodeId())
	if err != nil {
		return
	}
	s.refreshStandby(ctx, c, cfg, conn)
}

// failoverNodes 返回容器可能在的节点：所在节点在前，设置了顶替时再加上另一方（代理连接时按这个顺序找）。
func failoverNodes(c store.Container) []string {
	out := []string{c.NodeID}
	if cfg := failoverOf(c); cfg != nil {
		if st := standbyOf(cfg, c.NodeID).GetNodeId(); st != "" && st != c.NodeID {
			out = append(out, st)
		}
	}
	return out
}

// RelatedNodes 返回和项目有关的节点：容器所在的节点、顶替节点、作为见证人的节点、备份节点。
func (s *Service) RelatedNodes(ctx context.Context, projectID string) (map[string]bool, error) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range rows {
		out[c.NodeID] = true
		if c.BackupNodeID != "" {
			out[c.BackupNodeID] = true
		}
		if cfg := failoverOf(c); cfg != nil {
			for _, n := range []string{cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId(), cfg.GetWitness().GetNodeId()} {
				if n != "" {
					out[n] = true
				}
			}
		}
	}
	return out, nil
}

// ForgetHandedOver 移交以后，本面板不再是这些项目的见证人。
func (s *Service) ForgetHandedOver(ctx context.Context, projectID string) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return
	}
	for _, c := range rows {
		if cfg := failoverOf(c); cfg != nil && cfg.GetWitnessIsPanel() {
			s.Nodes.ClearWitness(c.ProjectID, c.Name)
		}
	}
}

// ReplicaInUse 判断节点上的副本还有没有用：容器以这个节点为备份节点，或者这个节点是它的热备。
func (s *Service) ReplicaInUse(ctx context.Context, nodeID, projectID, name string) bool {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return false
	}
	if c.BackupNodeID == nodeID {
		return true
	}
	cfg := failoverOf(c)
	return cfg != nil && c.NodeID != nodeID && isParticipant(cfg, nodeID)
}

// recentlyRebuilt 判断项目是不是刚从节点缓存拼回的（拼回还没完成，面板上的记录可能还缺东西）。
func recentlyRebuilt(p store.Project) bool {
	return p.RebuiltAt > 0 && time.Since(time.Unix(p.RebuiltAt, 0)) < 10*time.Minute
}

// witnessDisagrees 判断节点上报的"我是持有者、轮次 epoch"和面板作为见证人记下的对不上（节点可能被攻破，
// 随便报一个大轮次就能让面板把容器记到它那里）。面板不是见证人时没法核对，按节点的说法。
// 见证人的记录落后时（持有者靠另外两票拿到的租约），持有者下次续约会问到见证人，到时再改。
func (s *Service) witnessDisagrees(c store.Container, cfg *nagiv1.FailoverConfig, nodeID string, epoch uint64) bool {
	if !cfg.GetWitnessIsPanel() {
		return false
	}
	// 见证人是别的面板（例如换面板、提升以前的那个）时，本地留着的票不作数。
	m := s.Nodes.Mesh()
	if m == nil {
		return false
	}
	if self := m.PublicKey(); !bytes.Equal(cfg.GetWitness().GetKey(), self[:]) {
		return false
	}
	holder, recorded, ok := s.Nodes.WitnessRecord(c.ProjectID, c.Name)
	if !ok {
		return false
	}
	key := cfg.GetA().GetKey()
	if cfg.GetB().GetNodeId() == nodeID {
		key = cfg.GetB().GetKey()
	}
	// 见证人记录的轮次更新时，上报的已经过时；更旧时等见证人跟上。
	return recorded != epoch || !bytes.Equal(holder, key)
}
