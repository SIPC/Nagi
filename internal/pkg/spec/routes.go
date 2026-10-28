package spec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
)

// 端口映射（见 docs/exposure.md）：属于容器，存在面板上，开在容器所在的节点上，经 Caddy 对外开放。

// 对外开放的协议。
const (
	ProtoHTTP = "http"
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
)

var (
	domainRe    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	routeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// NormalizeRoute 整理一条映射：去掉空白、域名小写去重、http 绑了域名时不用对外端口、单个容器的服务名就是容器名。
func NormalizeRoute(r *nagiv1.Exposure) *nagiv1.Exposure {
	out := &nagiv1.Exposure{
		Name: strings.TrimSpace(r.GetName()), Container: strings.TrimSpace(r.GetContainer()), Service: strings.TrimSpace(r.GetService()),
		Target: r.GetTarget(), Protocol: strings.ToLower(strings.TrimSpace(r.GetProtocol())), Published: r.GetPublished(),
		Domains: NormalizeDomains(r.GetDomains()), HttpOnly: r.GetHttpOnly(),
	}
	if out.Protocol == ProtoHTTP && len(out.Domains) > 0 {
		out.Published = 0
	}
	if out.Service == "" {
		out.Service = out.Container
	}
	return out
}

// NormalizeDomains 把域名转成小写、去掉空的和重复的。
func NormalizeDomains(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range list {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// CheckRoute 校验一条映射本身的格式。
func CheckRoute(r *nagiv1.Exposure) error {
	if r.GetName() != "" && !routeNameRe.MatchString(r.GetName()) {
		return Invalid("映射名只能包含小写字母、数字、- 和 _")
	}
	if strings.TrimSpace(r.GetContainer()) == "" {
		return Invalid("需要选择容器")
	}
	if strings.TrimSpace(r.GetService()) == "" {
		return Invalid("需要选择容器组里的容器")
	}
	if r.GetTarget() == 0 || r.GetTarget() > 65535 {
		return Invalid("容器 %s 的端口无效", r.GetService())
	}
	switch r.GetProtocol() {
	case ProtoHTTP:
		for _, d := range r.GetDomains() {
			if !domainRe.MatchString(d) {
				return Invalid("域名 %q 无效（只能用小写字母、数字和连字符）", d)
			}
		}
		if len(r.GetDomains()) == 0 {
			return checkPublished(r)
		}
	case ProtoTCP, ProtoUDP:
		if len(r.GetDomains()) > 0 {
			return Invalid("只有 http 才能绑定域名")
		}
		return checkPublished(r)
	default:
		return Invalid("开放方式只能是 http、tcp 或 udp")
	}
	return nil
}

func checkPublished(r *nagiv1.Exposure) error {
	p := r.GetPublished()
	if p == 0 || p > 65535 {
		return Invalid("容器 %s 需要填写节点上对外的端口", r.GetService())
	}
	if p == 80 || p == 443 {
		return Invalid("端口 80 和 443 留给域名访问，请换一个端口，或者绑定域名")
	}
	return nil
}

// NewRouteName 生成映射名，例如 web-80（单个容器 web）、umami-db-5432、web-80-2。taken 是项目里已有的映射名。
func NewRouteName(r *nagiv1.Exposure, taken map[string]bool) string {
	base := fmt.Sprintf("%s-%d", r.GetContainer(), r.GetTarget())
	if r.GetService() != r.GetContainer() {
		base = fmt.Sprintf("%s-%s-%d", r.GetContainer(), r.GetService(), r.GetTarget())
	}
	base = strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			return c
		}
		return '-'
	}, strings.ToLower(base))
	name := base
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

// PortKey 是端口在节点上的唯一标识，例如 tcp/8080。http 没绑域名时也占用 tcp 端口。
type PortKey struct {
	Proto string
	Port  uint32
}

// UsedPorts 返回映射占用的节点端口。
func UsedPorts(r *nagiv1.Exposure) []PortKey {
	switch {
	case r.GetProtocol() == ProtoUDP:
		return []PortKey{{ProtoUDP, r.GetPublished()}}
	case r.GetProtocol() == ProtoTCP || len(r.GetDomains()) == 0:
		return []PortKey{{ProtoTCP, r.GetPublished()}}
	}
	return nil
}

// httpPorts 是常见的网站端口（容器内）。9000（php-fpm 等）、443（容器内一般是 HTTPS）不算。
var httpPorts = map[uint32]bool{80: true, 8000: true, 8080: true, 8081: true, 8888: true, 3000: true, 3001: true,
	4000: true, 5000: true, 5173: true, 8096: true}

// ComposePort 是 compose 里写的一个对外端口，部署时导入成映射。
type ComposePort struct {
	// Key 标识这个端口：服务/容器端口/协议。导入过的不再导入。
	Key   string
	Route *nagiv1.Exposure
	// Guessed：按端口猜测是网站（按 http 开放）。
	Guessed bool
	// Skip 不为 nil 时不导入，原因写在部署日志里（用 i18n.Localize 按请求的语言显示）。
	Skip error
}

// ComposePorts 列出容器 compose 里的对外端口（host 网络的服务除外）：写了 app_protocol: http 或者是常见网站端口的按 http，
// 其余按 tcp/udp 原样开放。
func ComposePorts(container string, proj *types.Project) []ComposePort {
	var out []ComposePort
	for _, name := range proj.ServiceNames() {
		svc := proj.Services[name]
		if svc.NetworkMode == "host" {
			continue
		}
		for _, port := range svc.Ports {
			proto := ProtoTCP
			if port.Protocol == ProtoUDP {
				proto = ProtoUDP
			}
			cp := ComposePort{Key: fmt.Sprintf("%s/%d/%s", name, port.Target, proto)}
			published, _ := strconv.Atoi(strings.Split(port.Published, "-")[0])
			if published == 0 {
				cp.Skip = i18n.Errorf("compose 里 %s 的端口 %d 没有指定对外端口，未开放，可以手动添加映射", name, port.Target)
				out = append(out, cp)
				continue
			}
			r := &nagiv1.Exposure{Container: container, Service: name, Target: port.Target, Protocol: proto, Published: uint32(published)}
			if proto == ProtoTCP && (strings.EqualFold(port.AppProtocol, "http") || (port.AppProtocol == "" && httpPorts[port.Target])) {
				r.Protocol, cp.Guessed = ProtoHTTP, port.AppProtocol == ""
			}
			if err := CheckRoute(r); err != nil {
				cp.Skip = i18n.Errorf("compose 里 %s 的端口 %d 未开放：%v", name, port.Target, err)
			}
			cp.Route = r
			out = append(out, cp)
		}
	}
	return out
}

// RouteLabel 用于日志，例如 web:80 → example.com、umami/db:5432 → tcp/15432。
func RouteLabel(r *nagiv1.Exposure) string {
	target := fmt.Sprintf("%s:%d", r.GetContainer(), r.GetTarget())
	if r.GetService() != r.GetContainer() {
		target = fmt.Sprintf("%s/%s:%d", r.GetContainer(), r.GetService(), r.GetTarget())
	}
	if len(r.GetDomains()) > 0 {
		return target + " → " + strings.Join(r.GetDomains(), ", ")
	}
	return fmt.Sprintf("%s → %s/%d", target, r.GetProtocol(), r.GetPublished())
}
