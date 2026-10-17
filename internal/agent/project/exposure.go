package project

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
)

// 对外开放的协议。
const (
	ProtoHTTP = "http"
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
)

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// materializeExposures 返回完整的开放设置：用户已有的设置，加上 compose 里写了 ports、
// 但还没有对应开放设置的端口（按 tcp/udp 原样开放）。这样 compose 里的 ports 自动转成开放设置。
func materializeExposures(proj *types.Project, cfg *nagiv1.ProjectConfig) []*nagiv1.Exposure {
	out := append([]*nagiv1.Exposure(nil), cfg.GetExposures()...)
	has := map[string]bool{}
	for _, e := range out {
		has[e.GetService()+"/"+strconv.Itoa(int(e.GetTarget()))] = true
	}
	for _, name := range proj.ServiceNames() {
		svc := proj.Services[name]
		if svc.NetworkMode == "host" {
			continue
		}
		for _, p := range svc.Ports {
			key := name + "/" + strconv.Itoa(int(p.Target))
			if has[key] {
				continue
			}
			published, _ := strconv.Atoi(strings.Split(p.Published, "-")[0])
			if published == 0 {
				continue
			}
			proto := ProtoTCP
			if p.Protocol == ProtoUDP {
				proto = ProtoUDP
			}
			out = append(out, &nagiv1.Exposure{
				Service: name, Target: p.Target, Protocol: proto, Published: uint32(published),
			})
			has[key] = true
		}
	}
	return out
}

// checkExposures 校验一个项目自身的开放设置。
func checkExposures(proj *types.Project, exposures []*nagiv1.Exposure) error {
	tcpPorts, udpPorts, domains := map[uint32]bool{}, map[uint32]bool{}, map[string]bool{}
	for _, e := range exposures {
		svc, ok := proj.Services[e.GetService()]
		if !ok {
			return invalid("开放设置里的服务 %q 不在 compose 中", e.GetService())
		}
		if svc.NetworkMode == "host" {
			return invalid("服务 %s 使用 host 网络，端口直接对外，不能再经 Caddy 开放", e.GetService())
		}
		if e.GetTarget() == 0 || e.GetTarget() > 65535 {
			return invalid("服务 %s 的容器端口无效", e.GetService())
		}
		switch e.GetProtocol() {
		case ProtoHTTP:
			for _, d := range e.GetDomains() {
				if !domainRe.MatchString(d) {
					return invalid("域名 %q 无效（只能用小写字母、数字和连字符）", d)
				}
				if domains[d] {
					return invalid("域名 %s 重复", d)
				}
				domains[d] = true
			}
			if len(e.GetDomains()) == 0 {
				if err := checkPublished(e); err != nil {
					return err
				}
				if tcpPorts[e.GetPublished()] {
					return invalid("端口 %d 重复", e.GetPublished())
				}
				tcpPorts[e.GetPublished()] = true
			}
		case ProtoTCP, ProtoUDP:
			if len(e.GetDomains()) > 0 {
				return invalid("只有 http 才能绑定域名")
			}
			if err := checkPublished(e); err != nil {
				return err
			}
			used := tcpPorts
			if e.GetProtocol() == ProtoUDP {
				used = udpPorts
			}
			if used[e.GetPublished()] {
				return invalid("%s 端口 %d 重复", e.GetProtocol(), e.GetPublished())
			}
			used[e.GetPublished()] = true
		default:
			return invalid("开放方式只能是 http、tcp 或 udp")
		}
	}
	return nil
}

func checkPublished(e *nagiv1.Exposure) error {
	p := e.GetPublished()
	if p == 0 || p > 65535 {
		return invalid("服务 %s 需要填写节点上对外的端口", e.GetService())
	}
	if p == 80 || p == 443 {
		return invalid("端口 80 和 443 留给域名访问，请换一个端口，或者绑定域名")
	}
	return nil
}

// portKey 是端口在节点上的唯一标识，例如 tcp/8080。http 没绑域名时也占用 tcp 端口。
type portKey struct {
	proto string
	port  uint32
}

func usedPorts(exposures []*nagiv1.Exposure) map[portKey]bool {
	out := map[portKey]bool{}
	for _, e := range exposures {
		switch {
		case e.GetProtocol() == ProtoUDP:
			out[portKey{ProtoUDP, e.GetPublished()}] = true
		case e.GetProtocol() == ProtoTCP || len(e.GetDomains()) == 0:
			out[portKey{ProtoTCP, e.GetPublished()}] = true
		}
	}
	return out
}

func usedDomains(exposures []*nagiv1.Exposure) map[string]bool {
	out := map[string]bool{}
	for _, e := range exposures {
		for _, d := range e.GetDomains() {
			out[d] = true
		}
	}
	return out
}

// checkConflicts 检查开放设置和本节点其他项目、其他程序是否冲突。
func (m *Manager) checkConflicts(ctx context.Context, id string, exposures []*nagiv1.Exposure) error {
	ports, domains := usedPorts(exposures), usedDomains(exposures)
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return err
	}
	ownDeployed := map[portKey]bool{}
	for _, r := range rows {
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		var configs []*nagiv1.ProjectConfig
		if cfg, _, err := pdb.latest(ctx); err == nil {
			configs = append(configs, cfg)
		}
		if d, err := pdb.draft(ctx); err == nil && d != nil {
			configs = append(configs, d)
		}
		deployed, _ := pdb.deployed(ctx)
		name := pdb.meta(ctx, metaName)
		pdb.Close()

		if r.ID == id {
			if deployed != nil {
				ownDeployed = usedPorts(deployed.GetExposures())
			}
			continue
		}
		for _, cfg := range configs {
			for k := range usedPorts(cfg.GetExposures()) {
				if ports[k] {
					return invalid("%s 端口 %d 已被项目 %s 使用", k.proto, k.port, name)
				}
			}
			for d := range usedDomains(cfg.GetExposures()) {
				if domains[d] {
					return invalid("域名 %s 已被项目 %s 使用", d, name)
				}
			}
		}
	}
	// 本项目已经在用的端口由 Caddy 占着，不用再探测。
	for k := range ports {
		if ownDeployed[k] {
			continue
		}
		if !portFree(k) {
			return invalid("%s 端口 %d 已被节点上的其他程序占用", k.proto, k.port)
		}
	}
	return nil
}

// portFree 尝试监听端口，判断它是否空闲。
func portFree(k portKey) bool {
	addr := ":" + strconv.Itoa(int(k.port))
	if k.proto == ProtoUDP {
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

// exposureLabel 用于日志和错误信息，例如 web:80 → https://example.com。
func exposureLabel(e *nagiv1.Exposure) string {
	target := fmt.Sprintf("%s:%d", e.GetService(), e.GetTarget())
	if len(e.GetDomains()) > 0 {
		return target + " → " + strings.Join(e.GetDomains(), ", ")
	}
	return fmt.Sprintf("%s → %s/%d", target, e.GetProtocol(), e.GetPublished())
}
