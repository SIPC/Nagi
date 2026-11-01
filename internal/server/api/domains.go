package api

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
)

// 域名检查（见 docs/exposure.md#域名检查）：域名是否解析到入口节点、证书状态。只用来提示，不拦着保存：
// 前面有 CDN 时解析到的本来就不是节点，DNS 也可能是之后才改。

const (
	// 一次最多检查的映射数和每条映射的域名数。
	maxCheckRoutes  = 64
	maxCheckDomains = 16
	// dnsCacheFor 是解析结果的缓存时间：界面定时刷新，编辑时每改一次域名也查一次。
	dnsCacheFor = 30 * time.Second
	dnsTimeout  = 3 * time.Second
)

// lookupHost 解析域名，测试时替换。
var lookupHost = net.DefaultResolver.LookupHost

type dnsResult struct {
	addrs []string
	at    time.Time
}

var dnsCache = struct {
	sync.Mutex
	m map[string]dnsResult
}{m: map[string]dnsResult{}}

// resolve 解析域名，失败或者没有地址时返回空。
func resolve(ctx context.Context, domain string) []string {
	dnsCache.Lock()
	r, ok := dnsCache.m[domain]
	dnsCache.Unlock()
	if ok && time.Since(r.at) < dnsCacheFor {
		return r.addrs
	}
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	addrs, err := lookupHost(ctx, domain)
	if err != nil {
		addrs = nil
	}
	slices.Sort(addrs)
	dnsCache.Lock()
	defer dnsCache.Unlock()
	for d, x := range dnsCache.m {
		if time.Since(x.at) >= dnsCacheFor {
			delete(dnsCache.m, d)
		}
	}
	dnsCache.m[domain] = dnsResult{addrs, time.Now()}
	return addrs
}

// dnsState 比较域名解析到的地址和节点地址。节点地址是面板看到的来源地址：内网地址对不上时说明不了什么（节点可能在 NAT 后面）。
func dnsState(ctx context.Context, addrs []string, nodeAddress string) string {
	if len(addrs) == 0 {
		return "unresolved"
	}
	if nodeAddress == "" {
		return "unknown"
	}
	ip, err := netip.ParseAddr(nodeAddress)
	nodeIPs := []string{nodeAddress}
	if err != nil {
		// 面板的对外地址可以是域名。
		nodeIPs = resolve(ctx, nodeAddress)
	}
	for _, a := range addrs {
		if slices.Contains(nodeIPs, a) {
			return "ok"
		}
	}
	if err != nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return "unknown"
	}
	return "mismatch"
}

// certState 返回节点上这个域名的证书状态。
func certState(c *nagiv1.DomainEntryCheck, report *nagiv1.ReportRequest, domain string) {
	info := report.GetNode()
	if report == nil || !info.GetReportsCerts() || info.GetProxyDisabled() {
		c.Cert = "unknown"
		return
	}
	// 刚保存的映射，节点下一次上报才带上它：先算申请中。
	c.Cert = "pending"
	for _, x := range report.GetCerts() {
		if x.GetDomain() == domain {
			c.Cert, c.CertError, c.CertNotAfter = x.GetState(), x.GetError(), x.GetNotAfter()
			return
		}
	}
}

// RetryDomainCert 让入口节点立刻重新申请证书。只接受项目自己的映射、绑定的域名和它的入口节点。
func (s *projectService) RetryDomainCert(ctx context.Context, req *connect.Request[nagiv1.RetryDomainCertRequest]) (*connect.Response[nagiv1.RetryDomainCertResponse], error) {
	m := req.Msg
	rows, err := s.Q.ListRoutes(ctx, m.GetProjectId())
	if err != nil {
		return nil, toConnect(err)
	}
	i := slices.IndexFunc(rows, func(r store.Route) bool { return r.Name == m.GetRoute() })
	if i < 0 {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("映射不存在"))
	}
	r := projects.RouteProto(rows[i])
	c, err := s.Q.GetContainer(ctx, store.GetContainerParams{ProjectID: m.GetProjectId(), Name: r.GetContainer()})
	if err != nil {
		return nil, toConnect(err)
	}
	if !slices.Contains(r.GetDomains(), m.GetDomain()) || r.GetHttpOnly() || !slices.Contains(spec.EntryNodesOf(r, c.NodeID), m.GetNodeId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("这个节点上的这个域名不需要申请证书"))
	}
	conn, err := s.Nodes.Get(m.GetNodeId())
	if err != nil {
		return nil, toConnect(err)
	}
	if _, err := conn.Agent.RetryCerts(ctx, &nagiv1.RetryCertsRequest{Domains: []string{m.GetDomain()}}); err != nil {
		if isStatus(err) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(status.Convert(err).Message()))
		}
		return nil, toConnect(err)
	}
	return connect.NewResponse(&nagiv1.RetryDomainCertResponse{}), nil
}

func (s *projectService) CheckDomains(ctx context.Context, req *connect.Request[nagiv1.CheckDomainsRequest]) (*connect.Response[nagiv1.CheckDomainsResponse], error) {
	rows, err := s.Q.ListContainers(ctx, req.Msg.GetProjectId())
	if err != nil {
		return nil, toConnect(err)
	}
	nodeOf := map[string]string{}
	for _, c := range rows {
		nodeOf[c.Name] = c.NodeID
	}
	out := &nagiv1.CheckDomainsResponse{}
	routes := req.Msg.GetRoutes()
	if len(routes) > maxCheckRoutes {
		routes = routes[:maxCheckRoutes]
	}
	// 同一个节点只查一次名字和地址。
	nodes := map[string]store.Node{}
	for _, in := range routes {
		r := spec.NormalizeRoute(in)
		node, ok := nodeOf[r.GetContainer()]
		if !ok || r.GetProtocol() != spec.ProtoHTTP {
			continue
		}
		domains := r.GetDomains()
		if len(domains) > maxCheckDomains {
			domains = domains[:maxCheckDomains]
		}
		for _, d := range domains {
			if !spec.ValidDomain(d) {
				continue
			}
			check := &nagiv1.DomainCheck{Route: r.GetName(), Domain: d, Addresses: resolve(ctx, d)}
			for _, id := range spec.EntryNodesOf(r, node) {
				n, ok := nodes[id]
				if !ok {
					if n, err = s.Q.GetNode(ctx, id); err != nil {
						continue
					}
					nodes[id] = n
				}
				e := &nagiv1.DomainEntryCheck{NodeId: id, NodeName: n.Name, Cert: "unknown"}
				if conn, err := s.Nodes.Peek(id); err == nil {
					e.Address = s.nodeAddress(ctx, conn)
					if r.GetHttpOnly() {
						e.Cert = "not_needed"
					} else {
						certState(e, conn.LastReport(), d)
					}
				} else if r.GetHttpOnly() {
					e.Cert = "not_needed"
				}
				e.Dns = dnsState(ctx, check.GetAddresses(), e.GetAddress())
				check.Entries = append(check.Entries, e)
			}
			out.Domains = append(out.Domains, check)
		}
	}
	return connect.NewResponse(out), nil
}
