package project

import (
	"context"
	"net"
	"strconv"

	"github.com/SIPC/nagi/internal/agent/proxy"
)

// Routes 汇总本节点所有项目的映射文件，生成 Caddy 的转发规则，上游是正在运行的容器的地址。
// 无效的映射文件跳过。
func (m *Manager) Routes(ctx context.Context) ([]proxy.Route, error) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var routes []proxy.Route
	for _, r := range rows {
		files := readRoutes(paths{root: r.Dir}, nil)
		if len(files) == 0 {
			continue
		}
		containers, err := m.docker.Instances(ctx, projectLabel(r.ID))
		if err != nil {
			return nil, err
		}
		for _, e := range files {
			if e.GetError() != "" {
				continue
			}
			route := proxy.Route{
				Project:   r.ID,
				Name:      e.GetName(),
				Service:   e.GetContainer() + "/" + e.GetService(),
				Protocol:  e.GetProtocol(),
				Published: e.GetPublished(),
				Domains:   e.GetDomains(),
				HTTPOnly:  e.GetHttpOnly(),
			}
			// 优先用容器自己的默认网络里的地址（容器组），其次是项目内网（单个容器）。
			containerNet := composeProjectName(r.ID, e.GetContainer()) + "_default"
			for _, c := range containers {
				if c.Container != e.GetContainer() || c.Service != e.GetService() || c.State != "running" {
					continue
				}
				ip, ok := c.IPs[containerNet]
				if !ok {
					ip, ok = c.IPs[projectNetwork(r.ID)]
				}
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
