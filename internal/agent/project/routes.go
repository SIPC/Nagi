package project

import (
	"context"
	"net"
	"strconv"

	"github.com/SIPC/nagi/internal/agent/proxy"
)

// Routes 汇总本节点所有已部署项目的对外开放规则，上游是正在运行的容器的地址。
func (m *Manager) Routes(ctx context.Context) ([]proxy.Route, error) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var routes []proxy.Route
	for _, r := range rows {
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		cfg, err := pdb.deployed(ctx)
		pdb.Close()
		if err != nil || cfg == nil || len(cfg.GetExposures()) == 0 {
			continue
		}
		containers, err := m.docker.ProjectContainers(ctx, composeProjectName(r.ID))
		if err != nil {
			return nil, err
		}
		defaultNet := composeProjectName(r.ID) + "_default"
		for _, e := range cfg.GetExposures() {
			route := proxy.Route{
				Project:   r.ID,
				Service:   e.GetService(),
				Protocol:  e.GetProtocol(),
				Published: e.GetPublished(),
				Domains:   e.GetDomains(),
				HTTPOnly:  e.GetHttpOnly(),
			}
			for _, c := range containers {
				if c.Service != e.GetService() || c.State != "running" {
					continue
				}
				// 优先用项目默认网络里的地址。
				ip, ok := c.IPs[defaultNet]
				if !ok {
					for _, a := range c.IPs {
						ip, ok = a, true
						break
					}
				}
				if ok {
					route.Upstreams = append(route.Upstreams, net.JoinHostPort(ip.String(), strconv.Itoa(int(e.GetTarget()))))
				}
			}
			routes = append(routes, route)
		}
	}
	return routes, nil
}
