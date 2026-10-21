package projects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 面板把容器的完整配置（ContainerSpec）下发给它所在的节点，节点缓存起来（见 docs/panel.md#项目数据）。
// 每次修改都推一次；节点离线时等它上报，发现缓存的修订号和面板上的不一样再补推。

// HashToken 计算 webhook 令牌的哈希（数据库里只存哈希）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// offlineError 是节点不在线：errors.Is 能识别为 nodes.ErrOffline，消息里说明是哪个节点。
type offlineError struct{ name string }

func (e offlineError) Error() string        { return "节点 " + e.name + " 不在线" }
func (e offlineError) Is(target error) bool { return target == nodes.ErrOffline }

func (s *Service) nodeErr(ctx context.Context, nodeID string, err error) error {
	if errors.Is(err, nodes.ErrOffline) {
		return offlineError{s.NodeName(ctx, nodeID)}
	}
	return err
}

// specFor 组装容器的配置：version 是要运行的版本（0 表示还没部署过，配置用最新版本的），
// cfg、shared、images 为 nil 时按 version 的版本记录（或最新版本）取。
func (s *Service) specFor(ctx context.Context, p store.Project, c store.Container, version int64) (*nagiv1.ContainerSpec, error) {
	settings, err := s.settings(p)
	if err != nil {
		return nil, err
	}
	sec, err := s.secrets(c)
	if err != nil {
		return nil, err
	}
	docker, err := s.dockerConfig(p)
	if err != nil {
		return nil, err
	}
	sp := &nagiv1.ContainerSpec{
		ProjectId: p.ID, ProjectName: p.Name, NetworkName: p.NetworkName, Name: c.Name, Dir: c.Dir,
		Revision: c.Revision, ProjectRevision: p.Revision, Version: version,
		DeployKey: sec.DeployKey, DeployKeyPub: c.DeployKeyPub, DockerConfig: docker, BackupPassword: sec.BackupPassword,
		SnapshotIntervalMinutes: settings.GetSnapshotIntervalMinutes(), InternalPorts: settings.GetInternalPorts(),
	}
	var v store.Version
	if version > 0 {
		v, err = s.Q.GetVersion(ctx, store.GetVersionParams{ProjectID: p.ID, Container: c.Name, Version: version})
	} else {
		v, err = s.Q.LatestVersion(ctx, store.LatestVersionParams{ProjectID: p.ID, Container: c.Name})
	}
	if err != nil {
		return nil, err
	}
	if sp.Config, sp.SharedEnv, err = s.versionData(v); err != nil {
		return nil, err
	}
	if version > 0 {
		_ = json.Unmarshal([]byte(v.Images), &sp.Images)
	}
	routes, err := s.Q.ListRoutes(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if r.Container == c.Name {
			sp.Routes = append(sp.Routes, routeProto(r))
		}
	}
	sp.Stateful = s.stateful(ctx, sp.GetConfig(), c, sp.GetSharedEnv())
	if s.Fingerprint != nil {
		sp.Panel = s.Fingerprint()
	}
	if c.BackupNodeID != "" {
		if n, err := s.Q.GetNode(ctx, c.BackupNodeID); err == nil && len(n.MeshKey) > 0 {
			sp.Backup = backupTarget(n)
		}
	}
	return sp, nil
}

// push 把容器当前部署的配置推给它所在的节点。节点不在线时返回错误（上线后由 Sync 补推）。
func (s *Service) push(ctx context.Context, projectID, name string) error {
	p, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	sp, err := s.specFor(ctx, p, c, c.DeployedVersion)
	if err != nil {
		return err
	}
	// 设置了顶替的容器，热备上的配置也要跟着更新。
	s.pushStandby(ctx, c)
	agent, err := s.agent(ctx, c.NodeID)
	if err != nil {
		return err
	}
	_, err = agent.PutContainer(ctx, &nagiv1.PutContainerRequest{Spec: sp})
	return err
}

// pushProject 把项目里所有容器的配置推给各自的节点（项目设置改了以后）。不在线的节点等它上线后补推。
func (s *Service) pushProject(ctx context.Context, projectID string) {
	rows, err := s.Q.ListContainers(ctx, projectID)
	if err != nil {
		return
	}
	for _, c := range rows {
		if err := s.push(ctx, projectID, c.Name); err != nil && !errors.Is(err, nodes.ErrOffline) {
			s.Log.Warn("下发容器配置失败", "project", projectID, "container", c.Name, "err", err)
		}
	}
}

// Sync 在节点上报后调用：节点缓存的修订号和面板上的不一样（节点离线时改过配置），就补推。
// 节点上有、面板上没有的容器不动（面板丢失后由新面板接管时要用到这些缓存）。
func (s *Service) Sync(c *nodes.Conn, req *nagiv1.ReportRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	have := map[string]*nagiv1.ContainerStatus{}
	for _, cs := range req.GetContainers() {
		have[cs.GetProjectId()+"/"+cs.GetName()] = cs
	}
	// 节点上有面板不知道的容器（换了面板、换了主密钥）：从它的缓存把项目拼回来。
	if !c.Pending() {
		// 节点上的配置是别的面板写的（本面板离开期间节点归别的面板管过）：本面板的记录旧了，以节点的为准。
		if stale := s.staleProjects(ctx, c.NodeID, req.GetContainers()); len(stale) > 0 {
			s.adoptFrom(ctx, c, stale)
		} else if s.needsRebuild(ctx, req.GetContainers()) {
			s.rebuildFrom(ctx, c, false)
		}
	}
	rows, err := s.Q.ListNodeContainers(ctx, c.NodeID)
	if err != nil {
		return
	}
	s.syncFailovers(ctx, c, req.GetFailovers())
	s.mu.Lock()
	if s.synced == nil {
		s.synced = map[*nodes.Conn]bool{}
	}
	s.synced[c] = true
	live := map[*nodes.Conn]bool{}
	for _, x := range s.Nodes.Conns() {
		live[x] = true
	}
	for x := range s.synced {
		if !live[x] && x != c {
			delete(s.synced, x)
		}
	}
	s.mu.Unlock()
	if rows, err = s.Q.ListNodeContainers(ctx, c.NodeID); err != nil {
		return
	}
	s.retireMoved(ctx, c, req.GetContainers(), rows)
	s.saveDeclaredPorts(ctx, rows, have)
	projects := map[string]store.Project{}
	for _, r := range rows {
		p, ok := projects[r.ProjectID]
		if !ok {
			if p, err = s.Q.GetProject(ctx, r.ProjectID); err != nil {
				continue
			}
			projects[r.ProjectID] = p
		}
		cs := have[r.ProjectID+"/"+r.Name]
		// 节点缓存的配置是别的面板（拼回之前的旧面板、提升之前的主面板）写的，也重新推一次：
		// 缓存里记上本面板，下次就不会再被当成别的面板写的、又以节点为准重建一遍。
		ownCache := s.Fingerprint == nil || cs.GetPanel() == s.Fingerprint()
		if cs != nil && cs.GetRevision() == r.Revision && cs.GetProjectRevision() == p.Revision && ownCache {
			continue
		}
		// 设置了顶替的容器还没在这个节点上：节点正在（或将要）用副本恢复它，不能先放一份空的。
		if cs == nil && len(r.Failover) > 0 {
			continue
		}
		// 正在部署这个项目时跳过：部署完会推最新的配置。
		l := s.lock(r.ProjectID)
		if !l.TryLock() {
			continue
		}
		err := s.push(ctx, r.ProjectID, r.Name)
		l.Unlock()
		if err != nil {
			s.Log.Warn("补推容器配置失败", "project", r.ProjectID, "container", r.Name, "node", c.NodeID, "err", err)
		} else {
			s.Log.Info("已补推容器配置", "project", r.ProjectID, "container", r.Name, "node", c.NodeID)
		}
	}
	s.pushAllLinks(ctx)
}

// retireMoved 处理节点上报、但面板上已经换到别的节点的容器（原节点离线时在备份节点上恢复了，或者重新部署到了其他节点）：
// 让这个节点停止并删除它，目录改名保留。正在操作的项目跳过（例如迁移进行到一半，面板还没改所在节点）。
// 面板上没有这个项目的不动：可能是换了面板、项目还没拼回来。
func (s *Service) retireMoved(ctx context.Context, c *nodes.Conn, reported []*nagiv1.ContainerStatus, rows []store.Container) {
	here := map[string]bool{}
	for _, r := range rows {
		here[r.ProjectID+"/"+r.Name] = true
	}
	for _, cs := range reported {
		if here[cs.GetProjectId()+"/"+cs.GetName()] {
			continue
		}
		row, err := s.container(ctx, cs.GetProjectId(), cs.GetName())
		if err != nil || row.NodeID == c.NodeID {
			continue
		}
		// 设置了顶替的容器由节点之间的租约决定谁运行，被顶替的一方自己会退位。
		if cfg := failoverOf(row); cfg != nil && isParticipant(cfg, c.NodeID) {
			continue
		}
		l := s.lock(cs.GetProjectId())
		if !l.TryLock() {
			continue
		}
		// 拿到锁以后再确认一次。
		if row, err = s.container(ctx, cs.GetProjectId(), cs.GetName()); err == nil && row.NodeID != c.NodeID {
			_, err = c.Agent.DeleteContainer(ctx, &nagiv1.DeleteContainerRequest{ProjectId: cs.GetProjectId(), Container: cs.GetName(), Retire: true})
			if err != nil {
				s.Log.Warn("让原节点停止已换到其他节点的容器失败", "project", cs.GetProjectId(), "container", cs.GetName(), "node", c.NodeID, "err", err)
			} else {
				s.Log.Info("原节点上的容器已停止，目录改名保留", "project", cs.GetProjectId(), "container", cs.GetName(), "node", c.NodeID)
			}
		}
		l.Unlock()
	}
}

// ---- 转换 ----

func routeProto(r store.Route) *nagiv1.Exposure {
	e := &nagiv1.Exposure{
		Name: r.Name, Container: r.Container, Service: r.Service, Target: uint32(r.Target), Protocol: r.Protocol,
		Published: uint32(r.Published), HttpOnly: r.HttpOnly != 0,
	}
	_ = json.Unmarshal([]byte(r.Domains), &e.Domains)
	return e
}

func versionProto(v store.Version) *nagiv1.Version {
	out := &nagiv1.Version{
		Container: v.Container, Version: v.Version, CreatedAt: timestamppb.New(time.Unix(v.CreatedAt, 0)),
		HasFiles: v.HasFiles != 0, RollbackOf: v.RollbackOf, Commit: v.GitCommit,
	}
	if v.DeployedAt.Valid {
		out.DeployedAt = timestamppb.New(time.Unix(v.DeployedAt.Int64, 0))
	}
	_ = json.Unmarshal([]byte(v.Images), &out.Images)
	return out
}

func deploymentProto(d store.Deployment) *nagiv1.Deployment {
	out := &nagiv1.Deployment{
		Id: d.ID, Time: timestamppb.New(time.Unix(d.CreatedAt, 0)), Source: d.Source,
		Success: d.Success == 1, Error: d.Error, RestoreOf: d.RestoreOf,
	}
	_ = json.Unmarshal([]byte(d.Containers), &out.Containers)
	_ = json.Unmarshal([]byte(d.Versions), &out.Versions)
	return out
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// declaredPorts 是容器各服务声明的端口（服务名 → 端口），存在容器的记录里。
type declaredPorts map[string][]uint32

// saveDeclaredPorts 记下节点上报的、本来就在这个节点上的容器声明的端口：节点断开时，
// 别的节点上的项目内网代理照旧按它放（见 docs/agent.md#项目内网）。
func (s *Service) saveDeclaredPorts(ctx context.Context, rows []store.Container, have map[string]*nagiv1.ContainerStatus) {
	for _, r := range rows {
		cs := have[r.ProjectID+"/"+r.Name]
		if cs == nil {
			continue
		}
		d := declaredPorts{}
		for _, svc := range cs.GetServices() {
			ports := append(d[svc.GetName()], svc.GetPorts()...)
			slices.Sort(ports)
			d[svc.GetName()] = slices.Compact(ports)
		}
		b, _ := json.Marshal(d)
		if string(b) == r.DeclaredPorts {
			continue
		}
		if err := s.Q.SetContainerDeclaredPorts(ctx, store.SetContainerDeclaredPortsParams{DeclaredPorts: string(b), ProjectID: r.ProjectID, Name: r.Name}); err != nil {
			s.Log.Warn("保存容器声明的端口失败", "project", r.ProjectID, "container", r.Name, "err", err)
		}
	}
}
