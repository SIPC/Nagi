package project

import (
	"context"
	"net"
	"strconv"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// Routes 汇总本节点上所有容器的端口映射（面板下发的配置缓存），生成 Caddy 的转发规则，
// 上游是正在运行的容器的地址。
func (m *Manager) Routes(ctx context.Context) ([]proxy.Route, error) {
	instances, err := m.docker.Instances(ctx, LabelProject)
	if err != nil {
		return nil, err
	}
	var routes []proxy.Route
	for _, c := range m.List(ctx) {
		s := c.Spec
		for _, e := range s.GetRoutes() {
			if e.GetError() != "" {
				continue
			}
			route := proxy.Route{
				Project:   s.GetProjectId(),
				Name:      e.GetName(),
				Service:   e.GetContainer() + "/" + e.GetService(),
				Protocol:  e.GetProtocol(),
				Published: e.GetPublished(),
				Domains:   e.GetDomains(),
				HTTPOnly:  e.GetHttpOnly(),
			}
			// 优先用容器组自己的默认网络里的地址，其次是项目内网（单个容器）。
			groupNet := spec.ComposeProjectName(s.GetProjectId(), s.GetName()) + "_default"
			for _, in := range instances {
				if in.Project != s.GetProjectId() || in.Container != s.GetName() || in.Service != e.GetService() || in.State != "running" {
					continue
				}
				ip, ok := in.IPs[groupNet]
				if !ok {
					ip, ok = in.IPs[projectNetwork(s.GetProjectId())]
				}
				if !ok {
					for _, a := range in.IPs {
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

// checkNewPorts 检查配置里的映射要用的端口：本节点的映射还没在用的端口（Caddy 还没监听），尝试监听一下，
// 确认没有被节点上的其他程序（包括面板端口、sshd 等）占用。和其他映射是否冲突由面板检查。
func (m *Manager) checkNewPorts(ctx context.Context, s *nagiv1.ContainerSpec) error {
	inUse := map[spec.PortKey]bool{}
	for _, c := range m.List(ctx) {
		for _, r := range c.Spec.GetRoutes() {
			for _, k := range spec.UsedPorts(r) {
				inUse[k] = true
			}
		}
	}
	for _, r := range s.GetRoutes() {
		for _, k := range spec.UsedPorts(r) {
			if !inUse[k] && !portFree(k) {
				return invalid("%s 端口 %d 已被节点上的其他程序占用", k.Proto, k.Port)
			}
		}
	}
	return nil
}

// portFree 尝试监听端口，判断它是否空闲。
func portFree(k spec.PortKey) bool {
	addr := ":" + strconv.Itoa(int(k.Port))
	if k.Proto == spec.ProtoUDP {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}
