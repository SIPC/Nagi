package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"github.com/compose-spec/compose-go/v2/types"
)

// 端口映射（见 docs/exposure.md）：属于容器，存在面板上，开在容器所在的节点上，改了立即生效（推给节点）。

func (s *Service) stateful(ctx context.Context, cfg *nagiv1.ContainerConfig, c store.Container, shared []*nagiv1.EnvVar) bool {
	cp, err := spec.Load(ctx, cfg, c.Name, shared, checkData(c), spec.ComposeProjectName(c.ProjectID, c.Name))
	if err != nil {
		return false
	}
	return spec.IsStateful(cfg, cp, spec.PreservePaths(cfg, cp, checkData(c)))
}

// SaveRoute 新建或修改一条映射，立即生效。
func (s *Service) SaveRoute(ctx context.Context, projectID string, in *nagiv1.Exposure) (*nagiv1.Project, error) {
	if in == nil {
		return nil, invalid("缺少映射")
	}
	r := spec.NormalizeRoute(in)
	if err := spec.CheckRoute(r); err != nil {
		return nil, err
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	c, err := s.container(ctx, projectID, r.GetContainer())
	if err != nil {
		return nil, invalid("容器 %s 不存在", r.GetContainer())
	}
	if err := s.checkService(ctx, c, r.GetService()); err != nil {
		return nil, err
	}
	existing, err := s.Q.ListRoutes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var old *store.Route
	taken := map[string]bool{}
	for i, e := range existing {
		taken[e.Name] = true
		if e.Name == r.GetName() {
			old = &existing[i]
		}
	}
	if r.GetName() == "" {
		r.Name = spec.NewRouteName(r, taken)
	} else if old == nil {
		return nil, invalid("映射 %s 不存在", r.GetName())
	}
	if err := s.checkConflicts(ctx, c.NodeID, projectID, r.GetName(), r); err != nil {
		return nil, err
	}
	if err := s.upsertRoute(ctx, projectID, r); err != nil {
		return nil, err
	}
	_ = s.Q.BumpContainer(ctx, store.BumpContainerParams{ProjectID: projectID, Name: c.Name})
	// 映射原来属于另一个容器时，那个容器的缓存也要更新。
	if old != nil && old.Container != c.Name {
		_ = s.Q.BumpContainer(ctx, store.BumpContainerParams{ProjectID: projectID, Name: old.Container})
		_ = s.push(ctx, projectID, old.Container)
	}
	if err := s.push(ctx, projectID, c.Name); err != nil {
		// 节点检查端口失败（被其他程序占用）时撤销。
		if old != nil {
			_ = s.upsertRoute(ctx, projectID, routeProto(*old))
		} else {
			_ = s.Q.DeleteRoute(ctx, store.DeleteRouteParams{ProjectID: projectID, Name: r.GetName()})
		}
		_ = s.Q.BumpContainer(ctx, store.BumpContainerParams{ProjectID: projectID, Name: c.Name})
		return nil, s.nodeErr(ctx, c.NodeID, err)
	}
	return s.reload(ctx, projectID)
}

func (s *Service) upsertRoute(ctx context.Context, projectID string, r *nagiv1.Exposure) error {
	domains, _ := json.Marshal(r.GetDomains())
	if r.GetDomains() == nil {
		domains = []byte("[]")
	}
	return s.Q.UpsertRoute(ctx, store.UpsertRouteParams{
		ProjectID: projectID, Name: r.GetName(), Container: r.GetContainer(), Service: r.GetService(), Target: int64(r.GetTarget()),
		Protocol: r.GetProtocol(), Published: int64(r.GetPublished()), Domains: string(domains), HttpOnly: boolInt(r.GetHttpOnly()),
	})
}

// DeleteRoute 删除一条映射，立即生效。
func (s *Service) DeleteRoute(ctx context.Context, projectID, name string) (*nagiv1.Project, error) {
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()
	existing, err := s.Q.ListRoutes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.Name != name {
			continue
		}
		if err := s.Q.DeleteRoute(ctx, store.DeleteRouteParams{ProjectID: projectID, Name: name}); err != nil {
			return nil, err
		}
		_ = s.Q.BumpContainer(ctx, store.BumpContainerParams{ProjectID: projectID, Name: e.Container})
		if err := s.push(ctx, projectID, e.Container); err != nil {
			s.Log.Warn("删除映射后下发配置失败（节点上线后补推）", "project", projectID, "container", e.Container, "err", err)
		}
		return s.reload(ctx, projectID)
	}
	return nil, invalid("映射 %s 不存在", name)
}

// checkService 检查服务在容器的 compose（最新版本或草稿）里，且不是 host 网络。
func (s *Service) checkService(ctx context.Context, c store.Container, service string) error {
	var cfgs []*nagiv1.ContainerConfig
	if _, cfg, _, err := s.latest(ctx, c); err == nil {
		cfgs = append(cfgs, cfg)
	}
	if d, err := s.draft(c); err == nil && d != nil {
		cfgs = append(cfgs, d)
	}
	for _, cfg := range cfgs {
		cp, err := spec.Load(ctx, cfg, c.Name, nil, checkData(c), spec.ComposeProjectName(c.ProjectID, c.Name))
		if err != nil {
			continue
		}
		if svc, ok := cp.Services[service]; ok {
			if svc.NetworkMode == "host" {
				return invalid("容器 %s 使用 host 网络，端口直接对外，不能再经 Caddy 开放", service)
			}
			return nil
		}
	}
	return invalid("容器组 %s 里没有容器 %s", c.Name, service)
}

// checkConflicts 检查一条映射和同一节点上所有映射的端口、整个面板上的域名是否冲突。
// skipProject/skipName 是正在修改的这条映射本身。
func (s *Service) checkConflicts(ctx context.Context, nodeID, skipProject, skipName string, r *nagiv1.Exposure) error {
	for _, d := range r.GetDomains() {
		if s.ReservedDomain != nil && s.ReservedDomain(d) {
			return invalid("域名 %s 已经绑定给面板", d)
		}
	}
	ports := map[spec.PortKey]bool{}
	for _, k := range spec.UsedPorts(r) {
		ports[k] = true
	}
	all, err := s.Q.ListAllRoutes(ctx)
	if err != nil {
		return err
	}
	containers, err := s.Q.ListAllContainers(ctx)
	if err != nil {
		return err
	}
	nodeOf := map[string]string{}
	for _, c := range containers {
		nodeOf[c.ProjectID+"/"+c.Name] = c.NodeID
	}
	names := map[string]string{}
	for _, other := range all {
		if other.ProjectID == skipProject && other.Name == skipName {
			continue
		}
		name, ok := names[other.ProjectID]
		if !ok {
			name = other.ProjectID
			if p, err := s.Q.GetProject(ctx, other.ProjectID); err == nil {
				name = p.Name
			}
			names[other.ProjectID] = name
		}
		o := routeProto(other)
		for _, d := range o.GetDomains() {
			for _, mine := range r.GetDomains() {
				if d == mine {
					return invalid("域名 %s 已被项目 %s 的映射 %s 使用", d, name, other.Name)
				}
			}
		}
		if nodeOf[other.ProjectID+"/"+other.Container] != nodeID {
			continue
		}
		for _, k := range spec.UsedPorts(o) {
			if ports[k] {
				return invalid("%s 端口 %d 已被项目 %s 的映射 %s 使用（同一个节点）", k.Proto, k.Port, name, other.Name)
			}
		}
	}
	return nil
}

// importPorts 把容器 compose 里新出现的 ports 导入成映射：导入过的端口记在容器的记录里，用户改过或删掉的映射
// 不会被覆盖或加回来。和其他映射冲突的不导入，只在部署日志里提示。
func (s *Service) importPorts(ctx context.Context, p store.Project, c store.Container, cp *types.Project, out Output) error {
	imported := map[string]bool{}
	var list []string
	if err := json.Unmarshal([]byte(c.ImportedPorts), &list); err == nil {
		for _, k := range list {
			imported[k] = true
		}
	}
	existing, err := s.Q.ListRoutes(ctx, p.ID)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, e := range existing {
		taken[e.Name] = true
	}
	changed := false
	for _, port := range spec.ComposePorts(c.Name, cp) {
		if imported[port.Key] {
			continue
		}
		imported[port.Key], changed = true, true
		if port.Skip != "" {
			out(port.Skip)
			continue
		}
		r := port.Route
		if err := s.checkConflicts(ctx, c.NodeID, p.ID, "", r); err != nil {
			out(fmt.Sprintf("compose 里 %s 的端口 %d 未开放：%v", r.GetService(), r.GetTarget(), err))
			continue
		}
		r.Name = spec.NewRouteName(r, taken)
		taken[r.Name] = true
		if err := s.upsertRoute(ctx, p.ID, r); err != nil {
			return err
		}
		if port.Guessed {
			out("已按配置里的端口添加映射：" + spec.RouteLabel(r) + "（按端口猜测是网站，按 HTTP 开放；不是的话在端口映射里改成 TCP）")
		} else {
			out("已按配置里的端口添加映射：" + spec.RouteLabel(r))
		}
	}
	if !changed {
		return nil
	}
	list = list[:0]
	for k := range imported {
		list = append(list, k)
	}
	sort.Strings(list)
	data, _ := json.Marshal(list)
	return s.Q.SetContainerImportedPorts(ctx, store.SetContainerImportedPortsParams{ImportedPorts: string(data), ProjectID: p.ID, Name: c.Name})
}
