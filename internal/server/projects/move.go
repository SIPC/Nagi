package projects

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
)

// 备份节点、迁移、在备份节点上恢复、重新部署到其他节点（见 docs/snapshot.md#异地副本、docs/failover.md），
// 都按容器进行，由面板按顺序指挥节点。配置一直在面板上，节点之间只传数据（容器的快照仓库）。

// targetNode 返回在线、已批准、有 mesh 公钥的节点。
func (s *Service) targetNode(ctx context.Context, nodeID string) (*nodes.Conn, store.Node, error) {
	n, err := s.Q.GetNode(ctx, nodeID)
	if err != nil {
		return nil, n, notFoundError{"节点不存在"}
	}
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return nil, n, s.nodeErr(ctx, nodeID, err)
	}
	if len(n.MeshKey) == 0 {
		return nil, n, invalid("节点 %s 还没有加入 mesh", n.Name)
	}
	return conn, n, nil
}

func backupTarget(n store.Node) *nagiv1.BackupTarget {
	return &nagiv1.BackupTarget{NodeId: n.ID, Name: n.Name, Key: n.MeshKey}
}

// placeDir 返回容器在节点上的目录：dir 为空时用 <节点的项目根目录>/<网络名>/<容器名>。
func (s *Service) placeDir(ctx context.Context, conn *nodes.Conn, p store.Project, c store.Container, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = s.defaultDir(ctx, conn, conn.NodeID, p.ID, p.Name, c.Name)
	}
	if !strings.HasPrefix(dir, "/") {
		return "", invalid("容器目录必须是绝对路径")
	}
	dir = path.Clean(dir)
	if owner, ok := s.dirOwner(ctx, conn.NodeID, dir, p.ID, c.Name); ok {
		return "", invalid("这个节点上的容器 %s 已经在用目录 %s（或顶替时要用）", owner, dir)
	}
	return dir, nil
}

// checkPlacement 检查容器能不能放到节点 conn 的目录 dir 上：映射的端口和域名在面板上不冲突，节点上目录可用、端口没被占用。
// 返回容器在那个节点上的配置。
func (s *Service) checkPlacement(ctx context.Context, conn *nodes.Conn, p store.Project, c store.Container, dir string) (*nagiv1.ContainerSpec, error) {
	sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
	if err != nil {
		return nil, err
	}
	for _, r := range sp.GetRoutes() {
		if r.GetError() != "" {
			continue
		}
		if err := s.checkConflicts(ctx, conn.NodeID, p.ID, r.GetName(), r); err != nil {
			return nil, err
		}
	}
	sp.Dir = dir
	sp.Backup = nil
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := conn.Agent.CheckContainer(cctx, &nagiv1.PutContainerRequest{Spec: sp}); err != nil {
		return nil, err
	}
	return sp, nil
}

// acceptReplica 让目标节点登记接收容器的副本，只接受源节点的同步。
func (s *Service) acceptReplica(ctx context.Context, target *nodes.Conn, sp *nagiv1.ContainerSpec, source store.Node) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := target.Agent.AcceptReplica(cctx, &nagiv1.AcceptReplicaRequest{
		Spec: sp, SourceKey: source.MeshKey, SourceNodeId: source.ID, SourceName: source.Name,
	})
	return err
}

// SetBackupNode 设置或取消容器的备份节点（nodeID 为空表示取消）。
func (s *Service) SetBackupNode(ctx context.Context, projectID, name, nodeID string, deleteReplica bool) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return nil, err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if nodeID == c.BackupNodeID {
		return s.reload(ctx, projectID)
	}
	if nodeID != "" {
		if err := noFailover(c); err != nil {
			return nil, invalid("容器 %s 设置了顶替，顶替节点本身就有一份随时更新的副本，不需要备份节点", name)
		}
	}
	old := c.BackupNodeID
	if nodeID != "" {
		if nodeID == c.NodeID {
			return nil, invalid("备份节点不能是容器所在的节点")
		}
		target, _, err := s.targetNode(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		source, err := s.Q.GetNode(ctx, c.NodeID)
		if err != nil {
			return nil, err
		}
		if len(source.MeshKey) == 0 {
			return nil, invalid("节点 %s 还没有加入 mesh", source.Name)
		}
		sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
		if err != nil {
			return nil, err
		}
		if err := s.acceptReplica(ctx, target, sp, source); err != nil {
			return nil, fmt.Errorf("备份节点登记副本: %w", s.nodeErr(ctx, nodeID, err))
		}
	}
	if err := s.Q.SetContainerBackupNode(ctx, store.SetContainerBackupNodeParams{BackupNodeID: nodeID, ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	// 源节点不在线时，上线后由 Sync 补推。
	if err := s.push(ctx, projectID, name); err == nil && nodeID != "" {
		go s.syncBackup(context.Background(), projectID, name)
	}
	if old != "" && deleteReplica {
		if agent, err := s.agent(ctx, old); err == nil {
			_, _ = agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
		}
	}
	s.Log.Info("已设置备份节点", "project", projectID, "container", name, "node", nodeID)
	return s.reload(ctx, projectID)
}

func (s *Service) syncBackup(ctx context.Context, projectID, name string) error {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	if c.BackupNodeID == "" {
		return invalid("容器 %s 没有设置备份节点", name)
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	st, err := agent.SyncBackup(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
	if err != nil {
		return err
	}
	if st.GetLastError() != "" {
		return errors.New(st.GetLastError())
	}
	return nil
}

// SyncBackup 立即同步到备份节点，同步完才返回。
func (s *Service) SyncBackup(ctx context.Context, projectID, name string) (*nagiv1.Project, error) {
	if err := s.syncBackup(ctx, projectID, name); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// BackupSnapshots 列出备份节点上副本里的快照。
func (s *Service) BackupSnapshots(ctx context.Context, projectID, name string) ([]*nagiv1.Snapshot, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if c.BackupNodeID == "" {
		return nil, invalid("容器 %s 没有设置备份节点", name)
	}
	agent, err := s.agent(ctx, c.BackupNodeID)
	if err != nil {
		return nil, err
	}
	res, err := agent.ListReplicaSnapshots(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
	if err != nil {
		return nil, err
	}
	return res.GetSnapshots(), nil
}

// moved 记下容器换到了节点 nodeID 的目录 dir 上（备份节点设置清掉），更新各节点的代理计划。
func (s *Service) moved(ctx context.Context, projectID, name, nodeID, dir string) error {
	if err := s.Q.MoveContainer(ctx, store.MoveContainerParams{NodeID: nodeID, Dir: dir, ProjectID: projectID, Name: name}); err != nil {
		return err
	}
	s.refreshLinks()
	return nil
}

// RestoreFromBackup 原节点离线时，在备份节点上把容器恢复出来：数据来自副本，配置来自面板，按部署过的版本启动。
// 原节点之后回来时，由 Sync 让它停止并删除这个容器（目录改名保留）。
func (s *Service) RestoreFromBackup(ctx context.Context, projectID, name, snapshotID, dir string, out Output) error {
	ctx = context.WithoutCancel(ctx)
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	if c.BackupNodeID == "" {
		return invalid("容器 %s 没有设置备份节点", name)
	}
	if err := noFailover(c); err != nil {
		return err
	}
	if _, err := s.Nodes.Get(c.NodeID); err == nil {
		return invalid("容器所在的节点在线，请用" + `"迁移"` + "把容器换到备份节点，不会丢数据")
	}
	target, _, err := s.targetNode(ctx, c.BackupNodeID)
	if err != nil {
		return err
	}
	if dir, err = s.placeDir(ctx, target, p, c, dir); err != nil {
		return err
	}
	sp, err := s.checkPlacement(ctx, target, p, c, dir)
	if err != nil {
		return err
	}
	out("在节点 " + s.NodeName(ctx, target.NodeID) + " 上恢复容器 " + name)
	stream, err := target.Agent.RestoreReplica(ctx, &nagiv1.RestoreReplicaRequest{Spec: sp, SnapshotId: snapshotID})
	if err != nil {
		return err
	}
	res, err := recvResult(stream.Recv, out)
	if res.GetPlaced() {
		if merr := s.moved(ctx, projectID, name, target.NodeID, sp.GetDir()); merr != nil {
			return merr
		}
		s.Log.Info("已在备份节点上恢复容器", "project", projectID, "container", name, "node", target.NodeID)
	}
	if err != nil {
		return err
	}
	out("容器已在 " + s.NodeName(ctx, target.NodeID) + " 上运行。原节点回来后，它上面的这个容器会被停止，目录改名保留。")
	return nil
}

// Migrate 把容器从所在节点迁移到另一个节点（两个节点都在线），不丢数据：
// 目标节点登记副本 → 源节点同步、停止、保存最后的快照、再同步 → 目标节点恢复并启动 → 源节点删除缓存（目录保留）。
// 任何一步失败都撤销：删除目标节点上的副本，源节点重新启动容器。
func (s *Service) Migrate(ctx context.Context, projectID, name, nodeID, dir string, out Output) error {
	ctx = context.WithoutCancel(ctx) // 开始了就做完：浏览器断开也会走完撤销
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	if nodeID == c.NodeID {
		return invalid("容器已经在这个节点上")
	}
	if err := noFailover(c); err != nil {
		return err
	}
	source, err := s.Nodes.Get(c.NodeID)
	if err != nil {
		return fmt.Errorf("%w。原节点离线时用"+`"从备份节点恢复"`+"或"+`"重新部署到其他节点"`, s.nodeErr(ctx, c.NodeID, err))
	}
	sourceNode, err := s.Q.GetNode(ctx, c.NodeID)
	if err != nil {
		return err
	}
	if len(sourceNode.MeshKey) == 0 {
		return invalid("节点 %s 还没有加入 mesh", sourceNode.Name)
	}
	target, targetNode, err := s.targetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if dir, err = s.placeDir(ctx, target, p, c, dir); err != nil {
		return err
	}
	out("检查节点 " + targetNode.Name)
	sp, err := s.checkPlacement(ctx, target, p, c, dir)
	if err != nil {
		return err
	}
	if c.DeployedVersion == 0 {
		// 还没部署过：没有数据要搬，直接换节点。
		if err := s.moved(ctx, projectID, name, nodeID, dir); err != nil {
			return err
		}
		if err := s.push(ctx, projectID, name); err != nil {
			return err
		}
		_, _ = source.Agent.DeleteContainer(ctx, &nagiv1.DeleteContainerRequest{ProjectId: projectID, Container: name})
		out("容器还没有部署过，已换到 " + targetNode.Name)
		return nil
	}
	if err := s.acceptReplica(ctx, target, sp, sourceNode); err != nil {
		return fmt.Errorf("目标节点登记副本: %w", err)
	}
	undo := func(cause error) error {
		out("撤销迁移：" + cause.Error())
		_, _ = target.Agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
		if stream, err := source.Agent.MigrateResume(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name}); err == nil {
			if _, err := recvResult(stream.Recv, out); err != nil {
				out("警告：原节点重新启动容器失败：" + err.Error())
			}
		}
		return cause
	}
	stream, err := source.Agent.MigrateOut(ctx, &nagiv1.MigrateOutRequest{ProjectId: projectID, Container: name, Target: backupTarget(targetNode)})
	if err != nil {
		return undo(err)
	}
	if _, err := recvResult(stream.Recv, out); err != nil {
		return undo(err)
	}
	out("在 " + targetNode.Name + " 上恢复容器")
	rs, err := target.Agent.RestoreReplica(ctx, &nagiv1.RestoreReplicaRequest{Spec: sp})
	if err != nil {
		return undo(err)
	}
	res, err := recvResult(rs.Recv, out)
	if !res.GetPlaced() {
		if err == nil {
			err = errors.New("目标节点没有恢复出容器")
		}
		return undo(err)
	}
	// 容器已经在目标节点上（启动失败也算：数据都在那里，用户修改配置后重新部署即可）。
	if merr := s.moved(ctx, projectID, name, nodeID, sp.GetDir()); merr != nil {
		return merr
	}
	if _, derr := source.Agent.DeleteContainer(ctx, &nagiv1.DeleteContainerRequest{ProjectId: projectID, Container: name}); derr != nil {
		out("警告：原节点删除容器缓存失败：" + derr.Error())
	}
	s.Log.Info("容器已迁移", "project", projectID, "container", name, "from", c.NodeID, "to", nodeID)
	if err != nil {
		return err
	}
	out(fmt.Sprintf("已迁移到 %s。原节点上的目录 %s 保留，确认没问题后可以手动删除。", targetNode.Name, c.Dir))
	return nil
}

// RedeployElsewhere 原节点离线、又没有副本时的最后手段：把容器换到另一个节点重新部署。
// 运行数据和旧版本的文件都在原节点上，所以数据是空的，旧版本不能带文件回滚。
// 从 git 拉文件的、或者不用 data/ 里文件的容器直接部署；要用 data/ 里的文件又没有文件来源的，提示先上传文件。
func (s *Service) RedeployElsewhere(ctx context.Context, projectID, name, nodeID, dir string, out Output) error {
	ctx = context.WithoutCancel(ctx)
	deploy, opts, err := s.redeployMove(ctx, projectID, name, nodeID, dir, out)
	if err != nil || !deploy {
		return err
	}
	_, err = s.Deploy(ctx, projectID, []Target{{Container: name}}, opts, out)
	return err
}

func (s *Service) redeployMove(ctx context.Context, projectID, name, nodeID, dir string, out Output) (bool, DeployOptions, error) {
	var opts DeployOptions
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	p, err := s.project(ctx, projectID)
	if err != nil {
		return false, opts, err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return false, opts, err
	}
	if nodeID == c.NodeID {
		return false, opts, invalid("容器已经在这个节点上")
	}
	if err := noFailover(c); err != nil {
		return false, opts, err
	}
	if _, err := s.Nodes.Get(c.NodeID); err == nil {
		return false, opts, invalid("容器所在的节点在线，请用" + `"迁移"` + "，不会丢数据")
	}
	target, targetNode, err := s.targetNode(ctx, nodeID)
	if err != nil {
		return false, opts, err
	}
	if dir, err = s.placeDir(ctx, target, p, c, dir); err != nil {
		return false, opts, err
	}
	if _, err := s.checkPlacement(ctx, target, p, c, dir); err != nil {
		return false, opts, err
	}
	if err := s.moved(ctx, projectID, name, nodeID, dir); err != nil {
		return false, opts, err
	}
	// 旧版本的文件留在原节点上，回滚时不能带文件。
	if err := s.Q.ClearVersionFiles(ctx, store.ClearVersionFilesParams{ProjectID: projectID, Container: name}); err != nil {
		return false, opts, err
	}
	if err := s.push(ctx, projectID, name); err != nil {
		return false, opts, err
	}
	s.Log.Info("容器已换到其他节点重新部署", "project", projectID, "container", name, "from", c.NodeID, "to", nodeID)
	out("容器已换到 " + targetNode.Name + "，目录 " + dir)
	if c.DeployedVersion == 0 {
		out("容器还没有部署过，需要时再部署")
		return false, opts, nil
	}
	_, cfg, shared, err := s.latest(ctx, c)
	if err != nil {
		return false, opts, err
	}
	if cfg.GetSource().GetGitUrl() != "" {
		opts.Pull = true
		return true, opts, nil
	}
	moved := c
	moved.Dir = dir
	cp, err := spec.Load(ctx, cfg, name, shared, checkData(moved), spec.ComposeProjectName(projectID, name))
	if err == nil && len(spec.DataServices(cp, checkData(moved))) > 0 {
		out("这个容器要用 data/ 里的文件，但没有文件来源：请先上传文件再部署")
		return false, opts, nil
	}
	return true, opts, nil
}
