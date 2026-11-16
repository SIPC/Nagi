package agent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 容器出站限制（见 docs/agent.md#出站限制）：Nagi 管理的容器默认不能访问节点本机、私有网段和链路本地地址
// （包括云元数据地址 169.254.169.254）。节点建一张 nftables 表 inet nagi，只匹配 Nagi 管理的容器的地址，不影响
// 机器上的其他容器；访问本机地址在 input 链拦截，访问其他地址在 forward 链拦截，规则只做丢弃，不改 Docker 的规则。
// 被拦下（或仅记录模式下记下）的 TCP、UDP 连接写进动态集合 hits4、hits6（源地址 . 目标地址 . 目标端口，10 分钟后过期，
// 每秒最多 10 个），节点每 5 秒读一次，新出现的汇总成告警。不用内核日志：有的内核（以及容器里的网络命名空间）不输出
// netfilter 日志。

// privateV4、privateV6 是容器默认不能访问的网段：私有网段、共享地址、链路本地。
var (
	privateV4 = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}
	privateV6 = []string{"fc00::/7", "fe80::/10"}
)

// egressContainer 是一个受限制的容器：项目、容器名和地址。
type egressContainer struct {
	// name 是 Docker 的容器名，nagi 是 Nagi 的容器名（容器组是组名）。
	project, name, nagi string
	addrs               []netip.Addr
}

// egressState 是生成规则用的输入。
type egressState struct {
	containers []egressContainer
	projects   map[string]*nagiv1.Egress
	// local 是节点本机的所有地址（包括 Docker 网关）。不用 nftables 的 fib：有的内核没有这个模块。
	local []netip.Addr
	// strict 是接着拦截模式的容器的网桥（docker0、br-<网络 ID>），known 是这些网桥上所有容器（包括不归 Nagi 管的）的地址。
	// 从这些网桥进来、源地址不是任何容器的地址的连接一律丢弃：被攻破的容器带着 CAP_NET_RAW，能用同网段里没分配的地址
	// 或者 IPv6 链路本地地址收发，按源地址匹配的项目规则管不到它们（见 docs/agent.md#出站限制）。
	strict []string
	known  []netip.Addr
	// public 是节点对外开放的端口（Caddy 的 80、443 和 TCP/UDP 转发）。发往节点本机这些端口的连接放行：
	// 它们本来就对公网开放，容器访问自己或别的项目的域名时会连到节点自己的地址。
	public []spec.PortKey
}

// projectKey 是规则里用的项目短名：项目 ID 本来就是十六进制。
func projectKey(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}

// buildRuleset 生成 nft -f 的脚本：整张表一起替换（先建再删再建，一个事务里生效）。
func buildRuleset(st egressState) string {
	var b strings.Builder
	b.WriteString("table inet nagi\ndelete table inet nagi\ntable inet nagi {\n")
	fmt.Fprintf(&b, "\tset private4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { %s }\n\t}\n", strings.Join(privateV4, ", "))
	fmt.Fprintf(&b, "\tset private6 {\n\t\ttype ipv6_addr\n\t\tflags interval\n\t\telements = { %s }\n\t}\n", strings.Join(privateV6, ", "))
	var l4, l6 []string
	for _, a := range st.local {
		if a.Is4() {
			l4 = append(l4, a.String())
		} else {
			l6 = append(l6, a.String())
		}
	}
	for _, fam := range []string{"4", "6"} {
		typ := "ipv4_addr"
		if fam == "6" {
			typ = "ipv6_addr"
		}
		fmt.Fprintf(&b, "\tset hits%s {\n\t\ttype %s . %s . inet_service\n\t\tsize 65535\n\t\tflags dynamic,timeout\n\t\ttimeout 10m\n\t}\n", fam, typ, typ)
	}
	var k4, k6 []string
	for _, a := range st.known {
		if a.Is4() {
			k4 = append(k4, a.String())
		} else {
			k6 = append(k6, a.String())
		}
	}
	for _, fam := range []string{"4", "6"} {
		typ, list, known := "ipv4_addr", l4, k4
		if fam == "6" {
			typ, list, known = "ipv6_addr", l6, k6
		}
		fmt.Fprintf(&b, "\tset local%s {\n\t\ttype %s\n", fam, typ)
		if len(list) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(list, ", "))
		}
		b.WriteString("\t}\n")
		fmt.Fprintf(&b, "\tset known%s {\n\t\ttype %s\n", fam, typ)
		if len(known) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(known, ", "))
		}
		b.WriteString("\t}\n")
	}
	var ifaces []string
	for _, i := range st.strict {
		ifaces = append(ifaces, strconv.Quote(i))
	}
	// 每个项目一组集合：项目里容器的地址。
	byProject := map[string][]egressContainer{}
	for _, c := range st.containers {
		byProject[c.project] = append(byProject[c.project], c)
	}
	ids := make([]string, 0, len(byProject))
	for id := range byProject {
		if spec.EgressMode(st.projects[id]) != spec.EgressOff {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var v4, v6 []string
		for _, c := range byProject[id] {
			for _, a := range c.addrs {
				if a.Is4() {
					v4 = append(v4, a.String())
				} else {
					v6 = append(v6, a.String())
				}
			}
		}
		k := projectKey(id)
		fmt.Fprintf(&b, "\tset p%s_4 {\n\t\ttype ipv4_addr\n", k)
		if len(v4) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(v4, ", "))
		}
		b.WriteString("\t}\n")
		fmt.Fprintf(&b, "\tset p%s_6 {\n\t\ttype ipv6_addr\n", k)
		if len(v6) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(v6, ", "))
		}
		b.WriteString("\t}\n")
	}
	chain := func(name, hook string, local bool) {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority -1; policy accept;\n", name, hook)
		// 已建立的连接（包括节点主动连到容器的回包）不管。
		b.WriteString("\t\tct state established,related accept\n")
		if !local {
			// 容器之间经 Docker 网桥的流量（项目内网）不管，网络之间的隔离由 Docker 负责。
			b.WriteString("\t\toifname { \"docker0\", \"br-*\" } accept\n")
		}
		// 源地址不是任何容器的地址：伪造的，直接丢弃。只管 TCP、UDP，不影响 ARP、NDP 和 ICMP。
		if len(ifaces) > 0 {
			for _, fam := range []string{"4", "6"} {
				ipk := "ip"
				if fam == "6" {
					ipk = "ip6"
				}
				dst := fmt.Sprintf("%s daddr @private%s", ipk, fam)
				if local {
					dst = fmt.Sprintf("%s daddr @local%s", ipk, fam)
				}
				fmt.Fprintf(&b, "\t\tiifname { %s } %s saddr != @known%s %s meta l4proto { tcp, udp } drop\n", strings.Join(ifaces, ", "), ipk, fam, dst)
			}
		}
		if local {
			for _, proto := range []string{spec.ProtoTCP, spec.ProtoUDP} {
				var ports []string
				for _, p := range st.public {
					if p.Proto == proto {
						ports = append(ports, strconv.FormatUint(uint64(p.Port), 10))
					}
				}
				if len(ports) > 0 {
					fmt.Fprintf(&b, "\t\tmeta l4proto %s th dport { %s } accept\n", proto, strings.Join(ports, ", "))
				}
			}
		}
		for _, id := range ids {
			e := st.projects[id]
			mode := spec.EgressMode(e)
			k := projectKey(id)
			for _, fam := range []string{"4", "6"} {
				ipk := "ip"
				if fam == "6" {
					ipk = "ip6"
				}
				src := fmt.Sprintf("%s saddr @p%s_%s", ipk, k, fam)
				// 放行列表。
				for _, a := range e.GetAllow() {
					r, err := spec.ParseEgressAllow(a)
					if err != nil || r.Prefix.Addr().Is4() != (fam == "4") {
						continue
					}
					fmt.Fprintf(&b, "\t\t%s %s daddr %s meta l4proto { tcp, udp } th dport %d accept\n", src, ipk, r.Prefix.String(), r.Port)
				}
				dst := fmt.Sprintf("%s daddr @private%s", ipk, fam)
				if local {
					dst = fmt.Sprintf("%s daddr @local%s", ipk, fam)
				}
				fmt.Fprintf(&b, "\t\t%s %s meta l4proto { tcp, udp } limit rate 10/second update @hits%s { %s saddr . %s daddr . th dport }\n", src, dst, fam, ipk, ipk)
				if mode == spec.EgressBlock {
					fmt.Fprintf(&b, "\t\t%s %s drop\n", src, dst)
				}
			}
		}
		b.WriteString("\t}\n")
	}
	chain("forward", "forward", false)
	chain("input", "input", true)
	b.WriteString("}\n")
	return b.String()
}

// egressStateOf 列出节点上 Nagi 管理的容器，和代理计划里各项目的出站限制合起来。
func egressStateOf(ctx context.Context, dc *docker.Client, plan *nagiv1.LinkPlan) (egressState, error) {
	st := egressState{projects: map[string]*nagiv1.Egress{}}
	for _, p := range plan.GetEgress() {
		st.projects[p.GetProjectId()] = p.GetEgress()
	}
	list, err := dc.ManagedInstances(ctx, project.LabelProject)
	if err != nil {
		return st, err
	}
	// 容器接着的 bridge 网络：IPv6 地址（容器列表里只有 IPv4）、网桥接口名、网桥上所有容器的地址。
	bridges := map[string]docker.BridgeNetwork{}
	for _, in := range list {
		if in.Project == "" || in.State != "running" {
			continue
		}
		for name := range in.IPs {
			if _, seen := bridges[name]; seen {
				continue
			}
			n, ok, err := dc.InspectBridge(ctx, name)
			if err != nil {
				return st, err
			}
			if ok {
				bridges[name] = n
			}
		}
	}
	strict, known := map[string]bool{}, map[netip.Addr]bool{}
	for _, in := range list {
		if in.Project == "" || in.State != "running" {
			continue
		}
		c := egressContainer{project: in.Project, name: in.Name, nagi: in.Container}
		seen := map[netip.Addr]bool{}
		addr := func(a netip.Addr) {
			if a = a.Unmap(); a.IsValid() && !seen[a] {
				seen[a] = true
				c.addrs = append(c.addrs, a)
			}
		}
		for name, a := range in.IPs {
			addr(a)
			if n, ok := bridges[name]; ok {
				for _, a := range n.Addrs[in.ID] {
					addr(a)
				}
				if spec.EgressMode(st.projects[in.Project]) == spec.EgressBlock {
					strict[name] = true
				}
			}
		}
		sort.Slice(c.addrs, func(i, j int) bool { return c.addrs[i].Less(c.addrs[j]) })
		if len(c.addrs) > 0 {
			st.containers = append(st.containers, c)
		}
	}
	for name := range strict {
		n := bridges[name]
		st.strict = append(st.strict, n.Iface)
		for _, list := range n.Addrs {
			for _, a := range list {
				known[a] = true
			}
		}
	}
	sort.Strings(st.strict)
	st.strict = slices.Compact(st.strict)
	for a := range known {
		st.known = append(st.known, a)
	}
	sort.Slice(st.known, func(i, j int) bool { return st.known[i].Less(st.known[j]) })
	sort.Slice(st.containers, func(i, j int) bool { return st.containers[i].name < st.containers[j].name })
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && !p.Addr().IsLoopback() {
				st.local = append(st.local, p.Addr().Unmap())
			}
		}
		sort.Slice(st.local, func(i, j int) bool { return st.local[i].Less(st.local[j]) })
	}
	return st, nil
}

// egressHit 是一条被拦下（或记下）的连接。
type egressHit struct {
	src, dst netip.Addr
	port     uint16
}

var hitRe = regexp.MustCompile(`([0-9a-fA-F.:]+) \. ([0-9a-fA-F.:]+) \. (\d+)`)

// parseHits 解析 nft list set 输出里的元素：源地址 . 目标地址 . 端口。
func parseHits(out string) []egressHit {
	var hits []egressHit
	for _, m := range hitRe.FindAllStringSubmatch(out, -1) {
		src, err1 := netip.ParseAddr(m[1])
		dst, err2 := netip.ParseAddr(m[2])
		port, err3 := strconv.Atoi(m[3])
		if err1 == nil && err2 == nil && err3 == nil {
			hits = append(hits, egressHit{src: src, dst: dst, port: uint16(port)})
		}
	}
	return hits
}

// egressAlertWindow：同一个容器、同一个目标这么久之内只告警一次。
const egressAlertWindow = 10 * time.Minute

// egressAlerts 汇总拦下的连接。
type egressAlerts struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (e *egressAlerts) first(key string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		e.last = map[string]time.Time{}
	}
	if t, ok := e.last[key]; ok && now.Sub(t) < egressAlertWindow {
		return false
	}
	if len(e.last) > 10000 {
		for k, t := range e.last {
			if now.Sub(t) >= egressAlertWindow {
				delete(e.last, k)
			}
		}
	}
	e.last[key] = now
	return true
}
