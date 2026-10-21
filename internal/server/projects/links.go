package projects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 项目内网的跨节点部分（见 docs/agent.md#项目内网）：面板知道每个容器在哪个节点、开着哪些端口，
// 给每个节点算一份代理计划（LinkPlan）：本节点的项目内网里要为别的节点上的同项目容器放哪些代理，
// 以及哪些节点可以连进来访问本节点上的容器。计划变了才下发；节点缓存在本地，面板不在时照旧。

// target 是项目内网里的一个名字：单个容器是"容器名"，容器组里的是"服务.容器组"。
type target struct {
	project, container, name, nodeID string
	ports                            []uint32
	// nodes 是容器可能在的节点，所在节点在前（设置了顶替时还有另一方）。
	nodes []string
}

// targets 返回项目里每个容器在项目内网里的名字和开放的端口：容器声明的端口（节点上报的）加上项目设置里补的内部端口。
// host 网络的服务不在项目内网里，跳过。
func (s *Service) targets(ctx context.Context, p store.Project, rows []store.Container, reports map[string]map[string]*nagiv1.ContainerStatus) []target {
	settings, err := s.settings(p)
	if err != nil {
		return nil
	}
	internal := spec.InternalPortMap(settings.GetInternalPorts())
	var out []target
	for _, c := range rows {
		cfg, err := s.runningConfig(ctx, c)
		if err != nil {
			continue
		}
		cp, err := spec.Load(ctx, cfg, c.Name, settings.GetSharedEnv(), checkData(c), spec.ComposeProjectName(c.ProjectID, c.Name))
		if err != nil {
			continue
		}
		hostNet := spec.HostNetworkServices(cp)
		// 声明的端口：所在节点在线时用它最近一次上报的；不在线时用记下的（不能因为节点断开就撤掉别的节点上的代理）。
		declared := declaredPorts{}
		if cs := reports[c.NodeID][c.ProjectID+"/"+c.Name]; cs != nil {
			for _, svc := range cs.GetServices() {
				declared[svc.GetName()] = append(declared[svc.GetName()], svc.GetPorts()...)
			}
		} else if c.DeclaredPorts != "" {
			_ = json.Unmarshal([]byte(c.DeclaredPorts), &declared)
		}
		for _, svc := range cp.ServiceNames() {
			if slices.Contains(hostNet, svc) {
				continue
			}
			name := c.Name
			if !spec.IsSingle(cfg) {
				name = svc + "." + c.Name
			}
			ports := append(append([]uint32(nil), declared[svc]...), internal[name]...)
			slices.Sort(ports)
			out = append(out, target{project: c.ProjectID, container: c.Name, name: name, nodeID: c.NodeID, ports: slices.Compact(ports), nodes: failoverNodes(c)})
		}
	}
	return out
}

// runningConfig 返回容器正在运行的配置（部署过的版本），没部署过时用最新版本。
func (s *Service) runningConfig(ctx context.Context, c store.Container) (*nagiv1.ContainerConfig, error) {
	if v, err := s.deployed(ctx, c); err == nil && v != nil {
		cfg, _, err := s.versionData(*v)
		return cfg, err
	}
	_, cfg, _, err := s.latest(ctx, c)
	return cfg, err
}

// linkedMap 返回每个项目连接了哪些项目。
func (s *Service) linkedMap(ctx context.Context) (map[string][]string, error) {
	rows, err := s.Q.ListProjectLinks(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, l := range rows {
		out[l.A] = append(out[l.A], l.B)
		out[l.B] = append(out[l.B], l.A)
	}
	return out, nil
}

// plan 算出节点 nodeID 的代理计划：
//   - 本节点上有容器的项目，为它在别的节点上的容器放代理（名字是容器名），为已连接项目的每个容器放代理（名字是"容器.网络名"，
//     对方在本节点上时由本节点直接转发）；
//   - 本节点上的容器允许哪些节点连进来：同项目的容器、已连接项目的容器所在的节点。
func (s *Service) plan(ctx context.Context, nodeID string) (*nagiv1.LinkPlan, error) {
	// 本节点上有容器的项目，以及本节点是某个容器的热备的项目（顶替以后容器就在本节点上了）。
	all, err := s.Q.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}
	projectIDs := map[string]bool{}
	for _, c := range all {
		if slices.Contains(failoverNodes(c), nodeID) {
			projectIDs[c.ProjectID] = true
		}
	}
	linked, err := s.linkedMap(ctx)
	if err != nil {
		return nil, err
	}
	// 各节点最近一次上报的容器状态（声明的端口），以及 mesh 公钥。
	reports := map[string]map[string]*nagiv1.ContainerStatus{}
	keys := map[string][]byte{}
	names := map[string]string{}
	nodes, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		keys[n.ID], names[n.ID] = n.MeshKey, n.Name
		m := map[string]*nagiv1.ContainerStatus{}
		if conn, err := s.Nodes.Peek(n.ID); err == nil {
			for _, cs := range conn.LastReport().GetContainers() {
				m[cs.GetProjectId()+"/"+cs.GetName()] = cs
			}
		}
		reports[n.ID] = m
	}
	// 用到的项目的信息只读一次。
	type info struct {
		p        store.Project
		rows     []store.Container
		settings *nagiv1.ProjectSettings
		targets  []target
	}
	cache := map[string]*info{}
	load := func(id string) *info {
		if i, ok := cache[id]; ok {
			return i
		}
		var i *info
		if p, err := s.Q.GetProject(ctx, id); err == nil {
			rows, err1 := s.Q.ListContainers(ctx, id)
			settings, err2 := s.settings(p)
			if err1 == nil && err2 == nil {
				i = &info{p: p, rows: rows, settings: settings, targets: s.targets(ctx, p, rows, reports)}
			}
		}
		cache[id] = i
		return i
	}
	proxy := func(projectID, alias string, t target) *nagiv1.LinkProxy {
		px := &nagiv1.LinkProxy{ProjectId: projectID, Alias: alias, TargetProject: t.project, TargetName: t.name, Ports: t.ports}
		for _, n := range t.nodes {
			if len(keys[n]) > 0 {
				px.Nodes = append(px.Nodes, &nagiv1.LinkNode{Key: keys[n], Name: names[n]})
			}
		}
		if len(t.ports) == 0 || len(px.Nodes) == 0 {
			return nil
		}
		return px
	}

	plan := &nagiv1.LinkPlan{}
	ids := make([]string, 0, len(projectIDs))
	for id := range projectIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		self := load(id)
		if self == nil {
			continue
		}
		allow := &nagiv1.LinkAllow{ProjectId: id, InternalPorts: self.settings.GetInternalPorts()}
		addCallers := func(rows []store.Container) {
			for _, c := range rows {
				for _, n := range failoverNodes(c) {
					if n != nodeID && len(keys[n]) > 0 && !containsKey(allow.Callers, keys[n]) {
						allow.Callers = append(allow.Callers, keys[n])
					}
				}
			}
		}
		// 别的节点上的同项目容器。
		for _, t := range self.targets {
			if t.nodeID != nodeID {
				if px := proxy(id, t.name, t); px != nil {
					plan.Proxies = append(plan.Proxies, px)
				}
			}
		}
		addCallers(self.rows)
		// 已连接的项目。
		others := append([]string(nil), linked[id]...)
		sort.Strings(others)
		for _, oid := range others {
			other := load(oid)
			if other == nil {
				continue
			}
			for _, t := range other.targets {
				if px := proxy(id, t.name+"."+other.p.NetworkName, t); px != nil {
					plan.Proxies = append(plan.Proxies, px)
				}
			}
			addCallers(other.rows)
		}
		if len(allow.Callers) > 0 {
			plan.Allows = append(plan.Allows, allow)
		}
	}
	return plan, nil
}

func containsKey(list [][]byte, k []byte) bool {
	for _, x := range list {
		if bytes.Equal(x, k) {
			return true
		}
	}
	return false
}

type linkSent struct {
	conn *nodes.Conn
	sum  [32]byte
}

// pushLinks 把节点的代理计划下发给它。和上次在这条连接上下发的一样时不发。
func (s *Service) pushLinks(ctx context.Context, conn *nodes.Conn) {
	plan, err := s.plan(ctx, conn.NodeID)
	if err != nil {
		s.Log.Warn("计算代理计划失败", "node", conn.NodeID, "err", err)
		return
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(plan)
	sum := sha256.Sum256(b)
	s.mu.Lock()
	if s.linkSent == nil {
		s.linkSent = map[string]linkSent{}
	}
	same := s.linkSent[conn.NodeID] == linkSent{conn, sum}
	s.mu.Unlock()
	if same {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := conn.Agent.SetLinks(ctx, plan); err != nil {
		s.Log.Warn("下发代理计划失败", "node", conn.NodeID, "err", err)
		return
	}
	s.mu.Lock()
	s.linkSent[conn.NodeID] = linkSent{conn, sum}
	s.mu.Unlock()
	s.Log.Info("已下发代理计划", "node", conn.NodeID, "proxies", len(plan.GetProxies()), "allows", len(plan.GetAllows()))
}

// pushAllLinks 给所有在线节点重新算一遍代理计划，变了的才下发。
// 节点每次上报后调用（上报里有容器声明的端口，会影响别的节点的计划），容器增删、项目设置改了以后也调用。
func (s *Service) pushAllLinks(ctx context.Context) {
	for _, c := range s.Nodes.Conns() {
		// 节点刚连上、第一次上报还没处理完时不推：面板上的记录可能是旧的（换过面板），要先以节点为准拼回。
		s.mu.Lock()
		ready := s.synced[c]
		s.mu.Unlock()
		if !c.Pending() && ready {
			s.pushLinks(ctx, c)
		}
	}
}

// refreshLinks 在后台给所有在线节点重新算代理计划（项目连接、容器增删、网络名改了以后），不等节点下次上报。
func (s *Service) refreshLinks() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushAllLinks(ctx)
	}()
}
