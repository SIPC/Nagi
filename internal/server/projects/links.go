package projects

import (
	"bytes"
	"context"
	"crypto/sha256"
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
		declared := map[string][]uint32{}
		if cs := reports[c.NodeID][c.ProjectID+"/"+c.Name]; cs != nil {
			for _, svc := range cs.GetServices() {
				declared[svc.GetName()] = append(declared[svc.GetName()], svc.GetPorts()...)
			}
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
			out = append(out, target{project: c.ProjectID, container: c.Name, name: name, nodeID: c.NodeID, ports: slices.Compact(ports)})
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

// plan 算出节点 nodeID 的代理计划。
func (s *Service) plan(ctx context.Context, nodeID string) (*nagiv1.LinkPlan, error) {
	local, err := s.Q.ListNodeContainers(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	projectIDs := map[string]bool{}
	for _, c := range local {
		projectIDs[c.ProjectID] = true
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
	plan := &nagiv1.LinkPlan{}
	ids := make([]string, 0, len(projectIDs))
	for id := range projectIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p, err := s.Q.GetProject(ctx, id)
		if err != nil {
			continue
		}
		rows, err := s.Q.ListContainers(ctx, id)
		if err != nil {
			continue
		}
		settings, err := s.settings(p)
		if err != nil {
			continue
		}
		// 本节点能访问别的节点上的同项目容器：每个名字放一个代理。
		callers := map[string]bool{}
		for _, t := range s.targets(ctx, p, rows, reports) {
			if t.nodeID == nodeID {
				continue
			}
			callers[t.nodeID] = true
			if len(t.ports) == 0 || len(keys[t.nodeID]) == 0 {
				continue
			}
			plan.Proxies = append(plan.Proxies, &nagiv1.LinkProxy{
				ProjectId: id, Alias: t.name, TargetProject: id, TargetName: t.name, Ports: t.ports,
				Nodes: []*nagiv1.LinkNode{{Key: keys[t.nodeID], Name: names[t.nodeID]}},
			})
		}
		// 别的节点上的同项目容器能访问本节点上的容器。
		allow := &nagiv1.LinkAllow{ProjectId: id, InternalPorts: settings.GetInternalPorts()}
		for _, c := range rows {
			if c.NodeID != nodeID && len(keys[c.NodeID]) > 0 && !containsKey(allow.Callers, keys[c.NodeID]) {
				allow.Callers = append(allow.Callers, keys[c.NodeID])
			}
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
		if !c.Pending() {
			s.pushLinks(ctx, c)
		}
	}
}
