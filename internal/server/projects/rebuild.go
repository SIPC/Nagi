package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 从节点的缓存把项目拼回来（见 docs/panel.md#项目数据）：新面板接管节点、或者换了主密钥以后，节点上报了面板不知道的容器，
// 面板向它要全部缓存（GetCache），按缓存里的配置建好项目和容器：
//   - 项目：名称、网络名、共享变量、快照间隔、镜像仓库认证取项目修订号最大的那份；内部端口取节点代理计划里的；
//   - 容器：所在节点和目录、当前部署的版本（配置、共享变量、镜像摘要）、端口映射、deploy key、快照仓库密码、备份节点、顶替设置；
//   - 项目连接：两边的项目都拼回来以后，按节点代理计划里"容器.网络名"的代理连上。
// 拼不回来的：草稿、旧版本和部署记录、webhook 令牌（重新生成，git 那边的 webhook 地址要更新）。

// rebuildWindow：项目从节点缓存拼回后多久内，别的节点缓存里这个项目的容器还能自动加回来。
const rebuildWindow = 24 * time.Hour

// needsRebuild 判断节点上报的容器里有没有面板不知道的。
func (s *Service) needsRebuild(ctx context.Context, report []*nagiv1.ContainerStatus) bool {
	for _, cs := range report {
		if _, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: cs.GetProjectId(), Name: cs.GetName()}); err != nil {
			return true
		}
	}
	return false
}

// staleProjects 返回节点上报的、配置由别的面板写的容器所属的项目（面板上有这个项目时）。
// 只看面板记录里本来就在这个节点上的容器：节点不能凭一句"配置是别的面板写的"让面板删掉别的节点上的项目。
// 写配置的那个面板节点还信任着时不算（两个面板同时管着这个节点），见 otherPanelActive。
func (s *Service) staleProjects(ctx context.Context, nodeID string, report []*nagiv1.ContainerStatus, trusted []string) []string {
	if s.Fingerprint == nil {
		return nil
	}
	self := s.ownPanels(ctx)
	seen := map[string]bool{}
	var out []string
	for _, cs := range report {
		if cs.GetPanel() == "" || self[cs.GetPanel()] || seen[cs.GetProjectId()] || slices.Contains(trusted, cs.GetPanel()) {
			continue
		}
		// 面板上没有这个容器：按拼回处理（例如项目刚从另一个节点以节点为准重建过，这个节点上的容器还没加回来）。
		c, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: cs.GetProjectId(), Name: cs.GetName()})
		if err != nil || !slices.Contains(failoverNodes(c), nodeID) {
			continue
		}
		s.mu.Lock()
		recent := time.Since(s.adoptedAt[cs.GetProjectId()]) < 2*time.Minute
		s.mu.Unlock()
		if recent {
			continue // 刚重建过，等配置推下去
		}
		seen[cs.GetProjectId()] = true
		out = append(out, cs.GetProjectId())
	}
	return out
}

// adoptFrom 以节点为准：删掉本面板上这些项目的旧记录（保留 webhook 令牌），再从节点的缓存拼回来。
// 项目在别的节点上的容器，等那些节点上报时同样拼回来。
func (s *Service) adoptFrom(ctx context.Context, conn *nodes.Conn, projectIDs []string) {
	for _, id := range projectIDs {
		l := s.lock(id)
		if !l.TryLock() {
			continue
		}
		rows, _ := s.Q.ListContainers(ctx, id)
		s.mu.Lock()
		if s.keepTokens == nil {
			s.keepTokens = map[string]string{}
		}
		for _, c := range rows {
			if sec, err := s.secrets(c); err == nil {
				s.keepTokens[id+"/"+c.Name] = sec.WebhookToken
			}
		}
		s.mu.Unlock()
		for _, c := range rows {
			if cfg := failoverOf(c); cfg != nil && cfg.GetWitnessIsPanel() {
				s.Nodes.ClearWitness(c.ProjectID, c.Name)
			}
		}
		err := s.Q.DeleteProject(ctx, id)
		s.mu.Lock()
		if s.adoptedAt == nil {
			s.adoptedAt = map[string]time.Time{}
		}
		s.adoptedAt[id] = time.Now()
		s.mu.Unlock()
		l.Unlock()
		if err != nil {
			s.Log.Warn("以节点为准时删除旧的项目记录失败", "project", id, "err", err)
			continue
		}
		s.Log.Warn("节点上的配置由别的面板写过，以节点为准重建项目", "project", id, "node", conn.NodeID)
		// 项目在别的节点上的容器也要重新拼回：那些连接下次上报时再读一遍缓存。
		s.mu.Lock()
		clear(s.rebuilt)
		s.mu.Unlock()
	}
	s.rebuildFrom(ctx, conn, true)
}

// rebuildFrom 向节点要全部缓存，把面板上没有的项目和容器拼回来。每条连接只做一次（force 时总是做）。
func (s *Service) rebuildFrom(ctx context.Context, conn *nodes.Conn, force bool) {
	s.mu.Lock()
	if s.rebuilt == nil {
		s.rebuilt = map[*nodes.Conn]bool{}
	}
	if s.rebuilt[conn] && !force {
		s.mu.Unlock()
		return
	}
	s.rebuilt[conn] = true
	live := map[*nodes.Conn]bool{}
	for _, c := range s.Nodes.Conns() {
		live[c] = true
	}
	for c := range s.rebuilt {
		if !live[c] && c != conn {
			delete(s.rebuilt, c) // 已经断开的连接
		}
	}
	s.mu.Unlock()
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cache, err := conn.Agent.GetCache(cctx, &nagiv1.GetCacheRequest{})
	cancel()
	if err != nil {
		s.Log.Warn("读取节点的配置缓存失败", "node", conn.NodeID, "err", err)
		s.mu.Lock()
		delete(s.rebuilt, conn) // 下次上报再试
		s.mu.Unlock()
		return
	}
	// 按项目分组，项目信息取项目修订号最大的那份。
	byProject := map[string][]*nagiv1.ContainerSpec{}
	for _, sp := range cache.GetContainers() {
		if checkSpecIDs(sp) != nil {
			continue
		}
		byProject[sp.GetProjectId()] = append(byProject[sp.GetProjectId()], sp)
	}
	internal := map[string][]string{}
	for _, a := range cache.GetLinks().GetAllows() {
		internal[a.GetProjectId()] = a.GetInternalPorts()
	}
	failovers := map[string]*nagiv1.FailoverStatus{}
	for _, st := range cache.GetFailovers() {
		if st.GetRole() == "holder" {
			failovers[st.GetConfig().GetProjectId()+"/"+st.GetConfig().GetContainer()] = st
		}
	}
	ids := make([]string, 0, len(byProject))
	for id := range byProject {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	changed := false
	unknown := map[string]*nagiv1.UnknownContainer{}
	defer func() {
		s.mu.Lock()
		if s.unknown == nil {
			s.unknown = map[string]map[string]*nagiv1.UnknownContainer{}
		}
		s.unknown[conn.NodeID] = unknown
		s.mu.Unlock()
	}()
	for _, id := range ids {
		specs := byProject[id]
		sort.Slice(specs, func(i, j int) bool { return specs[i].GetName() < specs[j].GetName() })
		created, err := s.rebuildProject(ctx, specs, internal[id])
		if err != nil {
			s.Log.Warn("从节点缓存拼回项目失败", "project", id, "node", conn.NodeID, "err", err)
			continue
		}
		if created {
			s.Log.Info("已从节点缓存拼回项目", "project", id, "name", specs[0].GetProjectName(), "node", conn.NodeID)
		}
		// 面板上本来就有的项目（不是刚从节点缓存拼回的）不接受节点缓存里多出来的容器：
		// 否则一个节点可以把自己的容器塞进别的项目，拿到它的共享变量、连进它的内网。
		// 这些容器列在节点页上，管理员确认后可以加入（AdoptContainer）。
		if p, err := s.project(ctx, id); err == nil && !created && time.Since(time.Unix(p.RebuiltAt, 0)) > rebuildWindow {
			for _, sp := range specs {
				if _, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: id, Name: sp.GetName()}); err != nil {
					s.Log.Warn("节点缓存里有项目不知道的容器，不自动加入（项目不是从节点缓存拼回的），可在节点页确认后加入", "project", id, "container", sp.GetName(), "node", conn.NodeID)
					unknown[id+"/"+sp.GetName()] = &nagiv1.UnknownContainer{ProjectId: id, ProjectName: p.Name, Container: sp.GetName()}
				}
			}
			continue
		}
		for _, sp := range specs {
			if _, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: id, Name: sp.GetName()}); err == nil {
				continue
			}
			if err := s.rebuildContainer(ctx, conn.NodeID, sp, failovers[id+"/"+sp.GetName()]); err != nil {
				s.Log.Warn("从节点缓存拼回容器失败", "project", id, "container", sp.GetName(), "node", conn.NodeID, "err", err)
				continue
			}
			changed = true
			s.Log.Info("已从节点缓存拼回容器", "project", id, "container", sp.GetName(), "node", conn.NodeID)
		}
	}
	// 项目连接：节点代理计划里为另一个项目的容器放的代理。
	for _, px := range cache.GetLinks().GetProxies() {
		a, b := px.GetProjectId(), px.GetTargetProject()
		if a == b {
			continue
		}
		if _, err := s.project(ctx, a); err != nil {
			continue
		}
		if _, err := s.project(ctx, b); err != nil {
			continue
		}
		linked, err := s.linkedMap(ctx)
		if err != nil || containsString(linked[a], b) {
			continue
		}
		x, y := linkPair(a, b)
		if err := s.Q.InsertProjectLink(ctx, store.InsertProjectLinkParams{A: x, B: y, CreatedAt: time.Now().Unix()}); err == nil {
			changed = true
		}
	}
	if changed {
		s.refreshLinks()
	}
}

func containsString(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

func checkSpecIDs(sp *nagiv1.ContainerSpec) error {
	if len(sp.GetProjectId()) != 16 || spec.CheckContainerName(sp.GetName()) != nil || sp.GetConfig() == nil {
		return errors.New("缓存的配置不完整")
	}
	return nil
}

// rebuildProject 建好项目（面板上还没有时），已有时按更新的项目修订号更新项目信息。返回是否新建。
func (s *Service) rebuildProject(ctx context.Context, specs []*nagiv1.ContainerSpec, internalPorts []string) (bool, error) {
	newest := specs[0]
	for _, sp := range specs {
		if sp.GetProjectRevision() > newest.GetProjectRevision() {
			newest = sp
		}
	}
	id := newest.GetProjectId()
	if _, err := s.project(ctx, id); err == nil {
		return false, nil
	}
	name := strings.TrimSpace(newest.GetProjectName())
	if name == "" {
		name = id
	}
	network := newest.GetNetworkName()
	if spec.CheckNetworkName(network) != nil {
		network = DefaultNetworkName(name)
	}
	// 网络名已经被别的项目用了（例如两个面板各自建过同名的项目）：加上项目 ID 的前几位。
	if other, err := s.Q.ProjectByNetworkName(ctx, network); err == nil && other != id {
		network = strings.TrimSuffix(network[:min(len(network), 27)], "-") + "-" + id[:4]
	}
	if len(newest.GetInternalPorts()) > 0 {
		internalPorts = newest.GetInternalPorts()
	}
	settings := &nagiv1.ProjectSettings{
		SharedEnv: newest.GetSharedEnv(), SnapshotIntervalMinutes: newest.GetSnapshotIntervalMinutes(), InternalPorts: internalPorts,
	}
	sealed, err := s.seal(settings, settingsCtx(id))
	if err != nil {
		return false, err
	}
	if err := s.Q.InsertProject(ctx, store.InsertProjectParams{ID: id, Name: name, NetworkName: network, CreatedAt: time.Now().Unix(), Settings: sealed}); err != nil {
		return false, err
	}
	if err := s.Q.SetProjectRebuiltAt(ctx, store.SetProjectRebuiltAtParams{RebuiltAt: time.Now().Unix(), ID: id}); err != nil {
		return false, err
	}
	if len(newest.GetDockerConfig()) > 0 {
		if err := s.saveDocker(ctx, id, newest.GetDockerConfig()); err != nil {
			s.Log.Warn("拼回项目的镜像仓库认证失败", "project", id, "err", err)
		}
	}
	return true, nil
}

// rebuildContainer 按节点缓存的配置在面板上建好容器（所在节点是上报的节点）。
func (s *Service) rebuildContainer(ctx context.Context, nodeID string, sp *nagiv1.ContainerSpec, fo *nagiv1.FailoverStatus) error {
	pid, name := sp.GetProjectId(), sp.GetName()
	rows, err := s.Q.ListContainers(ctx, pid)
	if err != nil {
		return err
	}
	position := int64(1)
	for _, r := range rows {
		position = max(position, r.Position+1)
	}
	s.mu.Lock()
	token, kept := s.keepTokens[pid+"/"+name]
	delete(s.keepTokens, pid+"/"+name)
	s.mu.Unlock()
	if !kept {
		var err error
		if token, err = spec.RandomHex(32); err != nil {
			return err
		}
	}
	secretJSON, _ := json.Marshal(secrets{WebhookToken: token, DeployKey: sp.GetDeployKey(), BackupPassword: sp.GetBackupPassword()})
	sealedSecrets, err := s.Sealer.Seal(secretJSON, []byte(secretsCtx(pid, name)))
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if err := s.Q.InsertContainer(ctx, store.InsertContainerParams{
		ProjectID: pid, Name: name, Position: position, NodeID: nodeID, Dir: sp.GetDir(), CreatedAt: now,
		WebhookHash: HashToken(token), Secrets: sealedSecrets, DeployKeyPub: sp.GetDeployKeyPub(),
	}); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = s.Q.DeleteContainer(ctx, store.DeleteContainerParams{ProjectID: pid, Name: name})
		return err
	}
	// 记下拼回的那份缓存的修订号：别的节点上报同一个容器的更新的一份时（例如旧的原节点先报上来），据此换成更新的那份。
	if sp.GetRevision() > 1 {
		if err := s.Q.SetContainerRevision(ctx, store.SetContainerRevisionParams{Revision: sp.GetRevision(), ProjectID: pid, Name: name}); err != nil {
			return fail(err)
		}
	}
	version := sp.GetVersion()
	if version == 0 {
		version = 1
	}
	sc, se, err := s.sealVersion(pid, name, version, sp.GetConfig(), sp.GetSharedEnv())
	if err != nil {
		return fail(err)
	}
	if err := s.Q.InsertVersion(ctx, store.InsertVersionParams{ProjectID: pid, Container: name, Version: version, Config: sc, SharedEnv: se, CreatedAt: now}); err != nil {
		return fail(err)
	}
	if sp.GetVersion() > 0 {
		images, _ := json.Marshal(sp.GetImages())
		// 版本文件在节点的快照仓库里，回滚时节点按版本号找。
		if err := s.Q.SetVersionDeployed(ctx, store.SetVersionDeployedParams{
			DeployedAt: sql.NullInt64{Int64: now, Valid: true}, Images: string(images), HasFiles: 1, ProjectID: pid, Container: name, Version: version,
		}); err != nil {
			return fail(err)
		}
		if err := s.Q.SetContainerDeployed(ctx, store.SetContainerDeployedParams{DeployedVersion: version, ProjectID: pid, Name: name}); err != nil {
			return fail(err)
		}
	}
	// 端口映射：只拼回这个容器自己的、名字没被占用、和别的映射不冲突的（不能借拼回抢别的容器的映射或域名）。
	existing, _ := s.Q.ListRoutes(ctx, pid)
	taken := map[string]bool{}
	for _, e := range existing {
		taken[e.Name] = true
	}
	for _, r := range sp.GetRoutes() {
		r = proto.Clone(r).(*nagiv1.Exposure)
		r.Container = name
		if r.GetName() == "" || taken[r.GetName()] {
			continue
		}
		if err := s.checkConflicts(ctx, nodeID, pid, "", r); err != nil {
			s.Log.Warn("拼回端口映射时有冲突，跳过", "project", pid, "container", name, "route", r.GetName(), "err", err)
			continue
		}
		if err := s.upsertRoute(ctx, pid, r); err != nil {
			s.Log.Warn("拼回端口映射失败", "project", pid, "container", name, "route", r.GetName(), "err", err)
			continue
		}
		taken[r.GetName()] = true
	}
	// compose 里现有的端口都算导入过：用户以前删掉的映射，下次部署时不会被加回来。
	if cp, err := spec.Load(ctx, sp.GetConfig(), name, sp.GetSharedEnv(), checkData(store.Container{Dir: sp.GetDir()}), spec.ComposeProjectName(pid, name)); err == nil {
		var keys []string
		for _, p := range spec.ComposePorts(name, cp) {
			keys = append(keys, p.Key)
		}
		raw, _ := json.Marshal(keys)
		_ = s.Q.SetContainerImportedPorts(ctx, store.SetContainerImportedPortsParams{ImportedPorts: string(raw), ProjectID: pid, Name: name})
	}
	if t := sp.GetBackup(); t != nil {
		if _, err := s.Q.GetNode(ctx, t.GetNodeId()); err == nil {
			_ = s.Q.SetContainerBackupNode(ctx, store.SetContainerBackupNodeParams{BackupNodeID: t.GetNodeId(), ProjectID: pid, Name: name})
		}
	}
	if fo != nil && fo.GetConfig() != nil {
		raw, _ := proto.Marshal(fo.GetConfig())
		_ = s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: raw, FailoverEpoch: int64(fo.GetEpoch()), ProjectID: pid, Name: name})
	}
	return nil
}

// adoptWitness 顶替设置里的见证人是别的面板（例如移交或拼回之前的旧面板）时，换成本面板。
// 换之前 A、B 两票照常构成多数，顶替不受影响。节点每次上报时检查，同一个容器每分钟最多试一次。
func (s *Service) adoptWitness(c store.Container, cfg *nagiv1.FailoverConfig) {
	m := s.Nodes.Mesh()
	if m == nil || !cfg.GetWitnessIsPanel() {
		return
	}
	if self := m.PublicKey(); string(cfg.GetWitness().GetKey()) == string(self[:]) {
		return
	}
	key := c.ProjectID + "/" + c.Name
	s.mu.Lock()
	if s.adoptTried == nil {
		s.adoptTried = map[string]time.Time{}
	}
	if time.Since(s.adoptTried[key]) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.adoptTried[key] = time.Now()
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.ChangeWitness(ctx, c.ProjectID, c.Name, ""); err != nil {
			s.Log.Warn("把见证人换成本面板失败，稍后重试", "project", c.ProjectID, "container", c.Name, "err", err)
			return
		}
		s.Log.Info("已把顶替的见证人换成本面板", "project", c.ProjectID, "container", c.Name)
	}()
}

// ownPanels 返回算作本面板写的配置的面板：本面板和同一组主备面板（现在的和曾经的，数据库是同一份，见 docs/standby.md）。
func (s *Service) ownPanels(ctx context.Context) map[string]bool {
	self := map[string]bool{}
	if s.Fingerprint != nil {
		self[s.Fingerprint()] = true
	}
	for _, fp := range standby.Members(ctx, s.Q) {
		self[fp] = true
	}
	return self
}

// otherPanelActive 判断节点上这个容器的配置是不是另一个面板写的、而节点现在还信任着那个面板：
// 两个面板同时管着这个节点（例如面板丢了以后让节点加入了新面板，旧面板又回来了）。这时本面板不以节点为准删记录，
// 也不把自己的配置推过去，免得两个面板轮流覆盖对方；记一条警告，等用户在节点上移除其中一个面板。
func otherPanelActive(cs *nagiv1.ContainerStatus, self map[string]bool, trusted []string) bool {
	return cs.GetPanel() != "" && !self[cs.GetPanel()] && slices.Contains(trusted, cs.GetPanel())
}

// UnknownContainers 返回节点上有、面板却不认识、没有自动加入的容器（最近一次读节点缓存时看到的）。
func (s *Service) UnknownContainers(ctx context.Context, nodeID string) []*nagiv1.UnknownContainer {
	s.mu.Lock()
	m := s.unknown[nodeID]
	s.mu.Unlock()
	var out []*nagiv1.UnknownContainer
	for _, u := range m {
		// 已经加入（或者同名的容器建好了）的不再列出。
		if _, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: u.GetProjectId(), Name: u.GetContainer()}); err == nil {
			continue
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetProjectName()+"/"+out[i].GetContainer() < out[j].GetProjectName()+"/"+out[j].GetContainer()
	})
	return out
}

// AdoptContainer 管理员确认后，按节点缓存里的配置把这个容器加入项目（所在节点就是这个节点）。
func (s *Service) AdoptContainer(ctx context.Context, nodeID, projectID, name string) error {
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	if _, err := s.project(ctx, projectID); err != nil {
		return err
	}
	if _, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: projectID, Name: name}); err == nil {
		return invalid("项目里已经有容器 %s", name)
	}
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return s.nodeErr(ctx, nodeID, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cache, err := conn.Agent.GetCache(cctx, &nagiv1.GetCacheRequest{})
	cancel()
	if err != nil {
		return err
	}
	var sp *nagiv1.ContainerSpec
	for _, x := range cache.GetContainers() {
		if x.GetProjectId() == projectID && x.GetName() == name && checkSpecIDs(x) == nil {
			sp = x
		}
	}
	if sp == nil {
		return invalid("节点上已经没有这个容器了")
	}
	var fo *nagiv1.FailoverStatus
	for _, st := range cache.GetFailovers() {
		if st.GetRole() == "holder" && st.GetConfig().GetProjectId() == projectID && st.GetConfig().GetContainer() == name {
			fo = st
		}
	}
	if err := s.rebuildContainer(ctx, nodeID, sp, fo); err != nil {
		return err
	}
	s.Log.Info("已按节点缓存把容器加入项目", "project", projectID, "container", name, "node", nodeID)
	s.refreshLinks()
	return nil
}
