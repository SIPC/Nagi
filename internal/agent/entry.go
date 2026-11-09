package agent

import (
	"context"
	"net"
	"slices"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 跨节点入口（见 docs/exposure.md#跨节点入口）：容器在别的节点上，外部请求从本节点的 Caddy 进来。
//   - 面板在代理计划（LinkPlan）里告诉本节点要开哪些映射；
//   - 每条映射在 127.0.0.1 上开一个随机端口作为 Caddy 的上游，收到的连接和数据报沿用项目内网的转发，
//     按计划里的顺序经 mesh 交给目标所在的节点，所以顶替以后不用改配置；
//   - 目标节点只放行计划（LinkEntryAllow）里写的那个端口。

// entryForward 是一条映射在本节点上的本地转发口。
type entryForward struct {
	addr  string
	close func()
}

// entryProxy 把计划里的入口转成项目内网的代理，转发时按它找目标。
func entryProxy(e *nagiv1.LinkEntry) *nagiv1.LinkProxy {
	return &nagiv1.LinkProxy{ProjectId: e.GetProjectId(), Alias: e.GetName(), TargetProject: e.GetProjectId(), TargetName: e.GetTargetName(), Nodes: e.GetNodes()}
}

// entryFor 找计划里的入口，已经不在计划里时返回 nil。
func (n *intranet) entryFor(projectID, name string) *nagiv1.LinkEntry {
	for _, e := range n.current().GetEntries() {
		if e.GetProjectId() == projectID && e.GetName() == name {
			return e
		}
	}
	return nil
}

func entryNetwork(e *nagiv1.LinkEntry) string {
	if e.GetProtocol() == spec.ProtoUDP {
		return spec.ProtoUDP
	}
	return spec.ProtoTCP
}

// validEntry 检查计划里的入口：只有 http、tcp、udp，端口都在范围内。
func validEntry(e *nagiv1.LinkEntry) bool {
	r := &nagiv1.Exposure{
		Container: e.GetContainer(), Service: e.GetService(), Target: e.GetTargetPort(), Protocol: e.GetProtocol(),
		Published: e.GetPublished(), Domains: e.GetDomains(),
	}
	return spec.CheckRoute(r) == nil && len(e.GetProjectId()) == 16 && e.GetName() != "" && e.GetTargetName() != ""
}

// entryRoutes 让本地转发口和计划一致，返回这些入口的 Caddy 规则。没有启动 Caddy 的节点不开。
func (n *intranet) entryRoutes() []proxy.Route {
	if n.a.cfg.DisableProxy {
		return nil
	}
	entries := n.current().GetEntries()
	n.mu.Lock()
	defer n.mu.Unlock()
	want := map[string]bool{}
	var routes []proxy.Route
	for _, e := range entries {
		if !validEntry(e) {
			n.a.log.Warn("忽略无效的入口", "project", e.GetProjectId(), "route", e.GetName())
			continue
		}
		key := e.GetProjectId() + "/" + e.GetName() + "/" + entryNetwork(e)
		want[key] = true
		f := n.entries[key]
		if f == nil {
			var err error
			if f, err = n.openEntry(e); err != nil {
				n.a.log.Warn("开入口的本地转发口失败", "project", e.GetProjectId(), "route", e.GetName(), "err", err)
				continue
			}
			n.entries[key] = f
		}
		routes = append(routes, proxy.Route{
			Project: e.GetProjectId(), Name: e.GetName(), Service: e.GetContainer() + "/" + e.GetService(), Protocol: e.GetProtocol(),
			Published: e.GetPublished(), Domains: e.GetDomains(), HTTPOnly: e.GetHttpOnly(), Upstreams: []string{f.addr},
		})
	}
	for key, f := range n.entries {
		if !want[key] {
			f.close()
			delete(n.entries, key)
		}
	}
	return routes
}

// openEntry 开一条入口的本地转发口。调用方持有 n.mu。
func (n *intranet) openEntry(e *nagiv1.LinkEntry) (*entryForward, error) {
	projectID, name := e.GetProjectId(), e.GetName()
	// 每次都按计划里最新的目标找：映射改了容器或端口时不用重开转发口。
	lookup := func() (*nagiv1.LinkProxy, uint16) {
		cur := n.entryFor(projectID, name)
		if cur == nil || entryNetwork(cur) != entryNetwork(e) {
			return nil, 0
		}
		return entryProxy(cur), uint16(cur.GetTargetPort())
	}
	if entryNetwork(e) == spec.ProtoUDP {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, err
		}
		u := newUDPProxy(pc, "", name, uint16(e.GetTargetPort()))
		// 代理和节点在同一个进程里，用 net.Pipe 代替 unixpacket：读缓冲足够大时，每次写正好对应一次读，数据报的边界还在。
		u.dial = func() (net.Conn, error) {
			a, b := net.Pipe()
			go func() {
				defer b.Close()
				n.forwardUDPEntry(b, lookup)
			}()
			return a, nil
		}
		go u.run()
		return &entryForward{addr: pc.LocalAddr().String(), close: u.close}, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				p, port := lookup()
				if p != nil {
					n.forward(c, c, p, port)
				}
			}()
		}
	}()
	return &entryForward{addr: ln.Addr().String(), close: func() { ln.Close() }}, nil
}

// forwardUDPEntry 处理入口的 UDP 转发口交过来的连接：第一条消息是"名字 端口"（这里用不上），之后和代理容器的一样。
func (n *intranet) forwardUDPEntry(c net.Conn, lookup func() (*nagiv1.LinkProxy, uint16)) {
	buf := make([]byte, 512)
	if _, err := c.Read(buf); err != nil {
		return
	}
	n.forwardUDP(c, lookup)
}

// entryAllowed 判断节点 k 是不是这个端口的入口节点。
func (n *intranet) entryAllowed(projectID, name string, port uint16, protocol string, k mesh.Key) bool {
	for _, a := range n.current().GetEntryAllows() {
		if a.GetProjectId() == projectID && a.GetTargetName() == name && a.GetPort() == uint32(port) && a.GetProtocol() == protocol &&
			slices.ContainsFunc(a.GetCallers(), func(c []byte) bool { return string(c) == string(k[:]) }) {
			return true
		}
	}
	return false
}

// entryPorts 返回本节点作为入口节点占用的端口。
func (n *intranet) entryPorts() []spec.PortKey {
	var out []spec.PortKey
	for _, e := range n.current().GetEntries() {
		out = append(out, spec.UsedPorts(&nagiv1.Exposure{Protocol: e.GetProtocol(), Published: e.GetPublished(), Domains: e.GetDomains()})...)
	}
	return out
}

// allRoutes 返回本节点要开的全部映射：容器在本节点上的，加上作为入口节点的。
// 同一条映射两边都有时（例如顶替过来的容器，本节点本来就是它的入口）只开一次，直接连容器。
func (a *agent) allRoutes(ctx context.Context) ([]proxy.Route, error) {
	routes, err := a.projects.Routes(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range routes {
		have[r.Project+"/"+r.Name] = true
	}
	for _, r := range a.intranet.entryRoutes() {
		if !have[r.Project+"/"+r.Name] {
			routes = append(routes, r)
		}
	}
	return routes, nil
}
