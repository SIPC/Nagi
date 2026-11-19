package projects

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 备份节点（见 docs/snapshot.md#异地副本）：一个容器可以有多个，按添加的顺序；可以和顶替同时设置。
// 源节点每次定时快照后依次同步到各个备份节点，备份节点只接受登记过的源节点（容器所在的节点，设置了顶替时还有另一个参与者）。

// supportsMultiBackup 判断节点支持多个备份节点、副本接受多个源节点（NodeInfo.multi_backup）。节点不在线时按上次上报的。
func (s *Service) supportsMultiBackup(ctx context.Context, nodeID string) bool {
	if conn, err := s.Nodes.Peek(nodeID); err == nil && conn.LastReport() != nil {
		return conn.LastReport().GetNode().GetMultiBackup()
	}
	n, err := s.Q.GetNode(ctx, nodeID)
	if err != nil {
		return false
	}
	info := &nagiv1.NodeInfo{}
	return proto.Unmarshal(n.Info, info) == nil && info.GetMultiBackup()
}

// backupSources 返回副本要接受同步的节点：容器所在的节点，设置了顶替时还有另一个参与者（它持有租约时由它同步）。
func (s *Service) backupSources(ctx context.Context, c store.Container) (store.Node, [][]byte, error) {
	source, err := s.Q.GetNode(ctx, c.NodeID)
	if err != nil {
		return source, nil, err
	}
	if len(source.MeshKey) == 0 {
		return source, nil, invalid("节点 %s 还没有加入节点网络", i18n.NodeRef(source.ID, source.Name))
	}
	var extra [][]byte
	if cfg := failoverOf(c); cfg != nil {
		for _, p := range []*nagiv1.FailoverParticipant{cfg.GetA(), cfg.GetB()} {
			if p.GetNodeId() != "" && p.GetNodeId() != c.NodeID {
				if n, err := s.Q.GetNode(ctx, p.GetNodeId()); err == nil && len(n.MeshKey) > 0 {
					extra = append(extra, n.MeshKey)
				}
			}
		}
	}
	return source, extra, nil
}

// registerBackup 让一个备份节点登记（或更新）接收容器的副本：只接受 backupSources 的同步。
// dir 是新登记时副本的存放位置（为空用节点的默认目录；已有的副本留在原处）。
func (s *Service) registerBackup(ctx context.Context, conn *nodes.Conn, p store.Project, c store.Container, dir string) error {
	source, extra, err := s.backupSources(ctx, c)
	if err != nil {
		return err
	}
	sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
	if err != nil {
		return err
	}
	return s.acceptReplica(ctx, conn, sp, source, extra, dir)
}

// registerBackups 更新容器所有备份节点上的登记：容器换了节点、设置或取消了顶替以后，源节点变了。
// 不在线的跳过，它上线时 registerBackupsOn 补上。
func (s *Service) registerBackups(ctx context.Context, projectID, name string) {
	p, err := s.project(ctx, projectID)
	if err != nil {
		return
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return
	}
	for _, id := range store.BackupNodes(c) {
		conn, err := s.Nodes.Get(id)
		if err != nil {
			continue
		}
		if err := s.registerBackup(ctx, conn, p, c, ""); err != nil {
			s.Log.Warn("更新备份节点上的副本登记失败", "project", projectID, "container", name, "node", id, "err", err)
		}
	}
}

// registerBackupsOn 节点连上面板时，更新它作为备份节点的副本登记（它离线期间容器可能换了节点或设置了顶替）。
func (s *Service) registerBackupsOn(ctx context.Context, conn *nodes.Conn) {
	projects, err := s.Q.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, p := range projects {
		rows, err := s.Q.ListContainers(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, c := range rows {
			if slices.Contains(store.BackupNodes(c), conn.NodeID) {
				if err := s.registerBackup(ctx, conn, p, c, ""); err != nil {
					s.Log.Warn("更新备份节点上的副本登记失败", "project", p.ID, "container", c.Name, "node", conn.NodeID, "err", err)
				}
			}
		}
	}
}

// checkBackupNodes 检查备份节点和顶替能不能同时用、多个备份节点各节点支不支持。nodes 是新的备份节点列表。
func (s *Service) checkBackupNodes(ctx context.Context, c store.Container, cfg *nagiv1.FailoverConfig, list []string) error {
	if len(list) == 0 {
		return nil
	}
	// 要按 backups 依次同步的节点：容器所在的节点，设置了顶替时还有另一个参与者。只有一个备份节点时旧版本也能同步。
	sources := []string{c.NodeID}
	if cfg != nil {
		sources = []string{cfg.GetA().GetNodeId(), cfg.GetB().GetNodeId()}
	}
	if len(list) > 1 {
		for _, id := range sources {
			if !s.supportsMultiBackup(ctx, id) {
				return invalid("节点 %s 版本过低，只能设置一个备份节点，请先升级节点", s.NodeRef(ctx, id))
			}
		}
	}
	// 设置了顶替时，备份节点要接受两个参与者的同步，旧版本的备份节点只认一个源节点。
	if cfg != nil {
		for _, id := range list {
			if !s.supportsMultiBackup(ctx, id) {
				return invalid("备份节点 %s 版本过低，不能和顶替同时使用，请先升级节点", s.NodeRef(ctx, id))
			}
		}
	}
	return nil
}

// SetBackupNodes 设置容器的备份节点（整个列表，按顺序；为空表示取消）。新加的备份节点要在线，登记后立即开始第一次同步；
// 去掉的备份节点上的副本在 deleteReplica 时删除（不在线的留着，可以在节点的「副本」里删）。
// dirs 是新加的备份节点上副本的存放位置（节点 ID → 目录，见 docs/snapshot.md#副本的存放位置），只有管理员能指定（接口里检查）。
func (s *Service) SetBackupNodes(ctx context.Context, projectID, name string, list []string, deleteReplica bool, dirs map[string]string) (*nagiv1.Project, error) {
	ctx = context.WithoutCancel(ctx)
	var clean []string
	for _, id := range list {
		if id != "" && !slices.Contains(clean, id) {
			clean = append(clean, id)
		}
	}
	list = clean
	if len(list) > 0 {
		if err := s.checkMovable(ctx, projectID, name); err != nil {
			return nil, err
		}
	}
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
	old := store.BackupNodes(c)
	if slices.Equal(old, list) {
		return s.reload(ctx, projectID)
	}
	cfg := failoverOf(c)
	for _, id := range list {
		if id == c.NodeID {
			return nil, invalid("备份节点不能是容器所在的节点")
		}
		if cfg != nil && isParticipant(cfg, id) {
			return nil, invalid("备份节点不能是顶替节点，顶替节点本身就保有一份副本")
		}
	}
	if err := s.checkBackupNodes(ctx, c, cfg, list); err != nil {
		return nil, err
	}
	var added, removed []string
	for _, id := range list {
		if !slices.Contains(old, id) {
			added = append(added, id)
		}
	}
	for _, id := range old {
		if !slices.Contains(list, id) {
			removed = append(removed, id)
		}
	}
	dirOf, err := s.checkReplicaDirs(ctx, added, dirs)
	if err != nil {
		return nil, err
	}
	for _, id := range added {
		target, _, err := s.targetNode(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.registerBackup(ctx, target, p, c, dirOf[id]); err != nil {
			return nil, i18n.Errorf("备份节点 %s 未能登记副本：%w", s.NodeRef(ctx, id), s.nodeErr(ctx, id, nodeMessage(err)))
		}
	}
	// 记下新加的备份节点单独指定的存放位置；去掉的备份节点的记录删掉。
	for _, id := range added {
		_ = s.Q.SetBackupReplicaDir(ctx, projectID, name, id, dirOf[id])
	}
	for _, id := range removed {
		_ = s.Q.SetBackupReplicaDir(ctx, projectID, name, id, "")
	}
	if err := s.Q.SetContainerBackupNode(ctx, store.SetContainerBackupNodeParams{BackupNodeID: store.JoinBackupNodes(list), ProjectID: projectID, Name: name}); err != nil {
		return nil, err
	}
	// 源节点不在线时，上线后由 Sync 补推。
	if err := s.push(ctx, projectID, name); err == nil && len(added) > 0 {
		go func() {
			if err := s.syncBackup(context.Background(), projectID, name, added...); err != nil {
				s.Log.Warn("开始第一次同步失败", "project", projectID, "container", name, "nodes", added, "err", err)
			}
		}()
	}
	if cfg != nil {
		s.pushStandby(ctx, c)
	}
	if deleteReplica {
		for _, id := range removed {
			if agent, err := s.agent(ctx, id); err == nil {
				_, _ = agent.DeleteReplica(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
			}
		}
	}
	s.refreshLinks()
	s.Log.Info("已设置备份节点", "project", projectID, "container", name, "nodes", list)
	return s.reload(ctx, projectID)
}

// supportsAsyncSync 判断节点支持后台同步、上报进度和停止同步（NodeInfo.async_backup_sync），按它在线时最近一次上报的。
func (s *Service) supportsAsyncSync(nodeID string) bool {
	conn, err := s.Nodes.Peek(nodeID)
	return err == nil && conn.LastReport().GetNode().GetAsyncBackupSync()
}

// oldNodeSyncWait 是旧版本的节点（同步完才返回）面板在后台最多等多久。等不及时取消，已经传完的文件留在备份节点上，下次接着传。
const oldNodeSyncWait = 24 * time.Hour

// syncStartTimeout 是请节点开始同步最多等多久（只是开始，不等同步结束）。
const syncStartTimeout = 30 * time.Second

// backupList 返回要同步的备份节点：ids 为空时容器所有的备份节点，否则确认它们都是。
func (s *Service) backupList(ctx context.Context, c store.Container, ids ...string) ([]string, error) {
	list := store.BackupNodes(c)
	if len(list) == 0 {
		return nil, invalid("容器 %s 没有设置备份节点", c.Name)
	}
	for _, id := range ids {
		if !slices.Contains(list, id) {
			return nil, invalid("节点 %s 不是容器 %s 的备份节点", s.NodeRef(ctx, id), c.Name)
		}
	}
	if len(ids) > 0 {
		return ids, nil
	}
	return list, nil
}

// nonEmpty 返回只含 id 的列表，id 为空时返回 nil（表示全部）。
func nonEmpty(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

// syncBackup 让源节点开始同步到备份节点 nodeID（为空时到所有备份节点，节点依次同步），不等同步结束：
// 同步在节点上跑完（面板这边不设时限），进度和结果随容器状态上报。旧版本的节点同步完才返回，面板在后台等，最多 oldNodeSyncWait。
// 设置了顶替时由持有租约、运行容器的节点同步（就是 c.NodeID）。
func (s *Service) syncBackup(ctx context.Context, projectID, name string, nodeIDs ...string) error {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	list, err := s.backupList(ctx, c, nodeIDs...)
	if err != nil {
		return err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	if s.supportsAsyncSync(c.NodeID) {
		ctx, cancel := context.WithTimeout(ctx, syncStartTimeout)
		defer cancel()
		var errs []error
		for _, id := range list {
			st, err := agent.SyncBackup(ctx, &nagiv1.SyncBackupRequest{ProjectId: projectID, Container: name, TargetNodeId: id, Async: true})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s.Nodes.PatchBackup(c.NodeID, projectID, name, st)
		}
		return errors.Join(errs...)
	}
	return s.waitOldSync(ctx, projectID, name, list, func(ctx context.Context, id string) error {
		_, err := agent.SyncBackup(ctx, &nagiv1.SyncBackupRequest{ProjectId: projectID, Container: name, TargetNodeId: id})
		return err
	})
}

// waitOldSync 在后台依次调用 call 同步到 list 里的备份节点（旧版本的源节点同步完才返回），最多等 oldNodeSyncWait。
// 每个（容器，备份节点）同时只有一个在等：已经在等的备份节点跳过（旧节点会让它结束后再同步一次，没有必要），
// 操作员连续点「立即同步」不会让面板积累协程、旧节点也不会一次接一次地做完整同步。都在等时返回「已在同步」。
func (s *Service) waitOldSync(ctx context.Context, projectID, name string, list []string, call func(ctx context.Context, id string) error) error {
	var claimed []string
	s.mu.Lock()
	if s.oldSyncs == nil {
		s.oldSyncs = map[string]bool{}
	}
	for _, id := range list {
		key := projectID + "/" + name + "/" + id
		if !s.oldSyncs[key] {
			s.oldSyncs[key] = true
			claimed = append(claimed, id)
		}
	}
	s.mu.Unlock()
	if len(claimed) == 0 {
		return invalid("已在同步")
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oldNodeSyncWait)
		defer cancel()
		for _, id := range claimed {
			if err := call(ctx, id); err != nil {
				s.Log.Warn("同步到备份节点失败", "project", projectID, "container", name, "node", id, "err", err)
			}
			s.mu.Lock()
			delete(s.oldSyncs, projectID+"/"+name+"/"+id)
			s.mu.Unlock()
		}
	}()
	return nil
}

// stopBackupSync 让源节点停止同步到备份节点 nodeID（为空时到所有备份节点的）。
func (s *Service) stopBackupSync(ctx context.Context, projectID, name, nodeID string) error {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	list, err := s.backupList(ctx, c, nonEmpty(nodeID)...)
	if err != nil {
		return err
	}
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	if !s.supportsAsyncSync(c.NodeID) {
		return invalid("节点 %s 版本过低，不能停止同步，请先升级节点", s.NodeRef(ctx, c.NodeID))
	}
	ctx, cancel := context.WithTimeout(ctx, syncStartTimeout)
	defer cancel()
	var errs []error
	for _, id := range list {
		st, err := agent.SyncBackup(ctx, &nagiv1.SyncBackupRequest{ProjectId: projectID, Container: name, TargetNodeId: id, Async: true, Cancel: true})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Nodes.PatchBackup(c.NodeID, projectID, name, st)
	}
	return errors.Join(errs...)
}

// StopBackupSync 停止同步到备份节点（nodeID 为空时停止到所有备份节点的）。
func (s *Service) StopBackupSync(ctx context.Context, projectID, name, nodeID string) (*nagiv1.Project, error) {
	if err := s.stopBackupSync(ctx, projectID, name, nodeID); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// SyncBackup 开始同步到备份节点（nodeID 为空时到所有备份节点），不等同步结束，见 syncBackup。
func (s *Service) SyncBackup(ctx context.Context, projectID, name, nodeID string) (*nagiv1.Project, error) {
	if err := s.syncBackup(ctx, projectID, name, nonEmpty(nodeID)...); err != nil {
		return nil, err
	}
	return s.reload(ctx, projectID)
}

// BackupSnapshots 列出备份节点 nodeID（为空时第一个）上副本里的快照。
func (s *Service) BackupSnapshots(ctx context.Context, projectID, name, nodeID string) ([]*nagiv1.Snapshot, error) {
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	nodeID, err = s.pickBackupNode(ctx, c, nodeID, false)
	if err != nil {
		return nil, err
	}
	agent, err := s.agent(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	res, err := agent.ListReplicaSnapshots(ctx, &nagiv1.ContainerRef{ProjectId: projectID, Container: name})
	if err != nil {
		return nil, err
	}
	return res.GetSnapshots(), nil
}

// pickBackupNode 确认 nodeID 是容器的备份节点。为空时：latest 为 false 用第一个；为 true 用在线的、最近同步成功的那个
// （按容器所在节点上次上报的同步状态），都没同步过时用第一个在线的。
func (s *Service) pickBackupNode(ctx context.Context, c store.Container, nodeID string, latest bool) (string, error) {
	list := store.BackupNodes(c)
	if len(list) == 0 {
		return "", invalid("容器 %s 没有设置备份节点", c.Name)
	}
	if nodeID != "" {
		if !slices.Contains(list, nodeID) {
			return "", invalid("节点 %s 不是容器 %s 的备份节点", s.NodeRef(ctx, nodeID), c.Name)
		}
		return nodeID, nil
	}
	if !latest {
		return list[0], nil
	}
	synced := map[string]int64{}
	for _, st := range s.reportedBackups(c) {
		synced[st.GetTargetNodeId()] = st.GetLastSync().GetSeconds()
	}
	best := ""
	for _, id := range list {
		if _, err := s.Nodes.Get(id); err != nil {
			continue
		}
		if best == "" || synced[id] > synced[best] {
			best = id
		}
	}
	if best == "" {
		return list[0], nil
	}
	return best, nil
}

// rememberBackups 记下节点上报的各容器同步到备份节点的状态（见 Service.backupReports）。只记 own 里的容器
// （面板上记着在这个节点上的）：节点不一定可信，不能借上报别的节点的容器，覆盖离线的源节点留下的同步记录，
// 让从备份恢复时选错副本。
func (s *Service) rememberBackups(nodeID string, list []*nagiv1.ContainerStatus, own map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backupReports == nil {
		s.backupReports = map[string]backupReport{}
	}
	for _, cs := range list {
		st := cs.GetBackups()
		if len(st) == 0 && cs.GetBackup() != nil {
			st = []*nagiv1.BackupStatus{cs.GetBackup()} // 旧版本的节点只报一个
		}
		key := cs.GetProjectId() + "/" + cs.GetName()
		if !own[key] {
			continue
		}
		if len(st) == 0 {
			if s.backupReports[key].node == nodeID {
				delete(s.backupReports, key)
			}
		} else {
			s.backupReports[key] = backupReport{node: nodeID, states: st}
		}
	}
}

// reportedBackups 返回容器所在节点最近一次上报的、同步到各个备份节点的状态（节点离线时是离线前的）。
// 同一个容器可能先后在几个节点上（迁移、顶替），只认现在所在节点报的：别的节点上的记录和现在的备份节点对不上。
func (s *Service) reportedBackups(c store.Container) []*nagiv1.BackupStatus {
	if conn, err := s.Nodes.Peek(c.NodeID); err == nil {
		for _, cs := range conn.LastReport().GetContainers() {
			if cs.GetProjectId() == c.ProjectID && cs.GetName() == c.Name {
				if len(cs.GetBackups()) > 0 {
					return cs.GetBackups()
				}
				if cs.GetBackup() != nil {
					return []*nagiv1.BackupStatus{cs.GetBackup()}
				}
				return nil
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.backupReports[c.ProjectID+"/"+c.Name]; r.node == c.NodeID {
		return r.states
	}
	return nil
}
