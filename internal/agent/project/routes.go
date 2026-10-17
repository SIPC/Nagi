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
		containers, err := m.docker.Containers(ctx, projectLabel(r.ID))
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
				Service:   e.GetMember() + "/" + e.GetService(),
				Protocol:  e.GetProtocol(),
				Published: e.GetPublished(),
				Domains:   e.GetDomains(),
				HTTPOnly:  e.GetHttpOnly(),
			}
			// 优先用成员自己的默认网络里的地址（compose 成员），其次是项目内网（单容器成员）。
			memberNet := composeProjectName(r.ID, e.GetMember()) + "_default"
			for _, c := range containers {
				if c.Member != e.GetMember() || c.Service != e.GetService() || c.State != "running" {
					continue
				}
				ip, ok := c.IPs[memberNet]
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
