package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// 端口映射：每条映射是项目目录 config/routes/<名字>.yml 一个文件。
// 节点汇总所有项目的映射生成 Caddy 配置；文件改了（界面上改或者直接在节点上改）立即生效，不需要重新部署。

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

func (p paths) routesDir() string { return filepath.Join(p.configDir(), "routes") }

// routeFile 是映射文件的格式。
type routeFile struct {
	Member    string   `yaml:"member"`
	Service   string   `yaml:"service,omitempty"`
	Port      uint32   `yaml:"port"`
	Protocol  string   `yaml:"protocol"`
	Published uint32   `yaml:"published,omitempty"`
	Domains   []string `yaml:"domains,omitempty"`
	HTTPOnly  bool     `yaml:"http_only,omitempty"`
}

const routeHeader = `# Nagi 端口映射。可以直接修改，保存后立即生效。
# member：成员名；service：服务名（单容器成员可以不写）；port：容器内端口；protocol：http、tcp 或 udp
# published：节点上对外的端口（tcp/udp 必填；http 没绑域名时必填）
# domains：绑定的域名（只用于 http，经 80/443 访问，拿到证书后自动跳转 HTTPS）
# http_only：只开 HTTP，不申请证书
`

// readRoutes 读取项目的所有映射。无效的文件也会返回，Error 里写明原因，不会生效。
// members 不为 nil 时，指向不存在的成员的映射也标为无效。
func readRoutes(p paths, members map[string]bool) []*nagiv1.Exposure {
	entries, err := os.ReadDir(p.routesDir())
	if err != nil {
		return nil
	}
	var out []*nagiv1.Exposure
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yml")
		if e.IsDir() || !ok {
			continue
		}
		r := &nagiv1.Exposure{Name: name}
		data, err := os.ReadFile(filepath.Join(p.routesDir(), e.Name()))
		var f routeFile
		if err == nil {
			err = yaml.Unmarshal(data, &f)
		}
		if err != nil {
			r.Error = "文件无法解析：" + err.Error()
			out = append(out, r)
			continue
		}
		r.Member, r.Service, r.Target, r.Protocol = f.Member, f.Service, f.Port, strings.ToLower(f.Protocol)
		r.Published, r.Domains, r.HttpOnly = f.Published, f.Domains, f.HTTPOnly
		if r.Service == "" {
			r.Service = r.Member // 单容器成员的服务名就是成员名
		}
		if err := checkRoute(r); err != nil {
			r.Error = err.Error()
		} else if members != nil && !members[r.Member] {
			r.Error = "成员 " + r.Member + " 不存在"
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	markDuplicates(out)
	return out
}

// markDuplicates 标出同一个项目里重复的端口和域名，重复的那条不生效。
func markDuplicates(routes []*nagiv1.Exposure) {
	ports, domains := map[portKey]string{}, map[string]string{}
	for _, r := range routes {
		if r.GetError() != "" {
			continue
		}
		for k := range usedPorts([]*nagiv1.Exposure{r}) {
			if other, ok := ports[k]; ok {
				r.Error = fmt.Sprintf("%s 端口 %d 和映射 %s 重复", k.proto, k.port, other)
			}
			ports[k] = r.GetName()
		}
		for _, d := range r.GetDomains() {
			if other, ok := domains[d]; ok {
				r.Error = fmt.Sprintf("域名 %s 和映射 %s 重复", d, other)
			}
			domains[d] = r.GetName()
		}
	}
}

// checkRoute 校验一条映射本身的格式。
func checkRoute(r *nagiv1.Exposure) error {
	if strings.TrimSpace(r.GetMember()) == "" {
		return invalid("需要填写成员名")
	}
	if strings.TrimSpace(r.GetService()) == "" {
		return invalid("需要填写服务名")
	}
	if r.GetTarget() == 0 || r.GetTarget() > 65535 {
		return invalid("服务 %s 的容器端口无效", r.GetService())
	}
	switch r.GetProtocol() {
	case ProtoHTTP:
		for _, d := range r.GetDomains() {
			if !domainRe.MatchString(d) {
				return invalid("域名 %q 无效（只能用小写字母、数字和连字符）", d)
			}
		}
		if len(r.GetDomains()) == 0 {
			return checkPublished(r)
		}
	case ProtoTCP, ProtoUDP:
		if len(r.GetDomains()) > 0 {
			return invalid("只有 http 才能绑定域名")
		}
		return checkPublished(r)
	default:
		return invalid("开放方式只能是 http、tcp 或 udp")
	}
	return nil
}

func checkPublished(r *nagiv1.Exposure) error {
	p := r.GetPublished()
	if p == 0 || p > 65535 {
		return invalid("服务 %s 需要填写节点上对外的端口", r.GetService())
	}
	if p == 80 || p == 443 {
		return invalid("端口 80 和 443 留给域名访问，请换一个端口，或者绑定域名")
	}
	return nil
}

func writeRoute(p paths, r *nagiv1.Exposure) error {
	if err := os.MkdirAll(p.routesDir(), 0o755); err != nil {
		return err
	}
	f := routeFile{
		Member: r.GetMember(), Service: r.GetService(), Port: r.GetTarget(), Protocol: r.GetProtocol(),
		Published: r.GetPublished(), Domains: r.GetDomains(), HTTPOnly: r.GetHttpOnly(),
	}
	if f.Service == f.Member {
		f.Service = ""
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(p.routesDir(), r.GetName()+".yml"), append([]byte(routeHeader), data...), 0o644)
}

// newRouteName 生成映射文件名，例如 web-80（单容器成员 web）、umami-db-5432、web-80-2。
func newRouteName(p paths, r *nagiv1.Exposure) string {
	base := fmt.Sprintf("%s-%d", r.GetMember(), r.GetTarget())
	if r.GetService() != r.GetMember() {
		base = fmt.Sprintf("%s-%s-%d", r.GetMember(), r.GetService(), r.GetTarget())
	}
	base = strings.ToLower(base)
	base = strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			return c
		}
		return '-'
	}, base)
	name := base
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(p.routesDir(), name+".yml")); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// ---- 冲突检查 ----

// portKey 是端口在节点上的唯一标识，例如 tcp/8080。http 没绑域名时也占用 tcp 端口。
type portKey struct {
	proto string
	port  uint32
}

func usedPorts(routes []*nagiv1.Exposure) map[portKey]bool {
	out := map[portKey]bool{}
	for _, r := range routes {
		if r.GetError() != "" {
			continue
		}
		switch {
		case r.GetProtocol() == ProtoUDP:
			out[portKey{ProtoUDP, r.GetPublished()}] = true
		case r.GetProtocol() == ProtoTCP || len(r.GetDomains()) == 0:
			out[portKey{ProtoTCP, r.GetPublished()}] = true
		}
	}
	return out
}

// checkConflicts 检查一条映射和本节点所有项目的其他映射、以及节点上其他程序是否冲突。
// skipProject/skipName 是正在修改的这条映射本身。
func (m *Manager) checkConflicts(ctx context.Context, skipProject, skipName string, r *nagiv1.Exposure) error {
	ports := usedPorts([]*nagiv1.Exposure{r})
	for _, d := range r.GetDomains() {
		if m.ReservedDomain != nil && m.ReservedDomain(d) {
			return invalid("域名 %s 已经绑定给面板", d)
		}
	}
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return err
	}
	inUse := map[portKey]bool{} // 已经由 Caddy 监听的端口，不用再探测
	for _, row := range rows {
		name := row.ID
		if pdb, err := openProject(ctx, row.Dir); err == nil {
			name = pdb.meta(ctx, metaName)
			pdb.Close()
		}
		for _, other := range readRoutes(paths{root: row.Dir}, nil) {
			if other.GetError() != "" {
				continue
			}
			for k := range usedPorts([]*nagiv1.Exposure{other}) {
				inUse[k] = true
			}
			if row.ID == skipProject && other.GetName() == skipName {
				continue
			}
			for k := range usedPorts([]*nagiv1.Exposure{other}) {
				if ports[k] {
					return invalid("%s 端口 %d 已被项目 %s 的映射 %s 使用", k.proto, k.port, name, other.GetName())
				}
			}
			for _, d := range other.GetDomains() {
				for _, mine := range r.GetDomains() {
					if d == mine {
						return invalid("域名 %s 已被项目 %s 的映射 %s 使用", d, name, other.GetName())
					}
				}
			}
		}
	}
	for k := range ports {
		if !inUse[k] && !portFree(k) {
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

// ---- 修改 ----

// SaveRoute 新建或修改一条映射，立即生效。
func (m *Manager) SaveRoute(ctx context.Context, id string, r *nagiv1.Exposure) (*nagiv1.Project, error) {
	if r == nil {
		return nil, invalid("缺少映射")
	}
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	p := paths{root: row.Dir}
	r = &nagiv1.Exposure{
		Name: r.GetName(), Member: strings.TrimSpace(r.GetMember()), Service: strings.TrimSpace(r.GetService()), Target: r.GetTarget(),
		Protocol: strings.ToLower(r.GetProtocol()), Published: r.GetPublished(),
		Domains: normalizeDomains(r.GetDomains()), HttpOnly: r.GetHttpOnly(),
	}
	if r.GetProtocol() == ProtoHTTP && len(r.GetDomains()) > 0 {
		r.Published = 0
	}
	if r.Service == "" {
		r.Service = r.Member
	}
	if err := checkRoute(r); err != nil {
		return nil, err
	}
	if r.GetName() == "" {
		r.Name = newRouteName(p, r)
	} else if !routeNameRe.MatchString(r.GetName()) {
		return nil, invalid("映射名只能包含小写字母、数字、- 和 _")
	}
	if err := m.checkService(ctx, row.Dir, id, r.GetMember(), r.GetService()); err != nil {
		return nil, err
	}
	if err := m.checkConflicts(ctx, id, r.GetName(), r); err != nil {
		return nil, err
	}
	if err := writeRoute(p, r); err != nil {
		return nil, err
	}
	return m.readProject(ctx, id, row.Dir)
}

func (m *Manager) readProject(ctx context.Context, id, dir string) (*nagiv1.Project, error) {
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	return pdb.project(ctx, id, dir)
}

// DeleteRoute 删除一条映射，立即生效。
func (m *Manager) DeleteRoute(ctx context.Context, id, name string) (*nagiv1.Project, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !routeNameRe.MatchString(name) {
		return nil, invalid("映射名无效")
	}
	if err := os.Remove(filepath.Join(paths{root: row.Dir}.routesDir(), name+".yml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m.readProject(ctx, id, row.Dir)
}

// checkService 检查成员存在、服务在它的 compose（最新版本或草稿）里，且不是 host 网络。
func (m *Manager) checkService(ctx context.Context, dir, id, member, service string) error {
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	if _, err := pdb.member(ctx, member); err != nil {
		return invalid("成员 %s 不存在", member)
	}
	var cfgs []*nagiv1.MemberConfig
	if cfg, _, err := pdb.latest(ctx, member); err == nil {
		cfgs = append(cfgs, cfg)
	}
	if d, err := pdb.draft(ctx, member); err == nil && d != nil {
		cfgs = append(cfgs, d)
	}
	shared := pdb.settings(ctx).GetSharedEnv()
	mp := paths{root: dir}.member(member)
	for _, cfg := range cfgs {
		cp, err := loadMember(ctx, cfg, member, shared, mp.dataDir(), composeProjectName(id, member))
		if err != nil {
			continue
		}
		if svc, ok := cp.Services[service]; ok {
			if svc.NetworkMode == "host" {
				return invalid("服务 %s 使用 host 网络，端口直接对外，不能再经 Caddy 开放", service)
			}
			return nil
		}
	}
	return invalid("服务 %s 不在成员 %s 的 compose 中", service, member)
}

func normalizeDomains(list []string) []string {
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

// ---- 从 compose 导入 ----

// importPorts 把成员 compose 里新出现的 ports 导入成映射文件：写了 app_protocol: http 或者是常见网站端口的按 http，其余按 tcp/udp 原样开放。
// 导入过的端口记在成员的记录里：用户改过或删掉的映射不会被覆盖或加回来。
// 和其他映射冲突的端口不导入，只在部署日志里提示。
func (m *Manager) importPorts(ctx context.Context, pdb *projectDB, p paths, id string, mrow projectstore.Member, proj *types.Project, onLine func(string)) error {
	imported := map[string]bool{}
	var list []string
	if err := json.Unmarshal([]byte(mrow.ImportedPorts), &list); err == nil {
		for _, k := range list {
			imported[k] = true
		}
	}
	changed := false
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
			key := fmt.Sprintf("%s/%d/%s", name, port.Target, proto)
			if imported[key] {
				continue
			}
			imported[key], changed = true, true
			published, _ := strconv.Atoi(strings.Split(port.Published, "-")[0])
			if published == 0 {
				onLine(fmt.Sprintf("compose 里 %s 的端口 %d 没有指定对外端口，未开放，可以手动添加映射", name, port.Target))
				continue
			}
			r := &nagiv1.Exposure{Member: mrow.Name, Service: name, Target: port.Target, Protocol: proto, Published: uint32(published)}
			guessed := false
			if proto == ProtoTCP && (strings.EqualFold(port.AppProtocol, "http") || (port.AppProtocol == "" && likelyHTTP(port.Target))) {
				r.Protocol, guessed = ProtoHTTP, port.AppProtocol == ""
			}
			if err := checkRoute(r); err != nil {
				onLine(fmt.Sprintf("compose 里 %s 的端口 %d 未开放：%v", name, port.Target, err))
				continue
			}
			if err := m.checkConflicts(ctx, id, "", r); err != nil {
				onLine(fmt.Sprintf("compose 里 %s 的端口 %d 未开放：%v", name, port.Target, err))
				continue
			}
			r.Name = newRouteName(p, r)
			if err := writeRoute(p, r); err != nil {
				return err
			}
			if guessed {
				onLine("已从 compose 导入映射：" + routeLabel(r) + "（按端口猜测是网站，按 HTTP 开放；不是的话在映射里改成 TCP）")
			} else {
				onLine("已从 compose 导入映射：" + routeLabel(r))
			}
		}
	}
	if !changed {
		return nil
	}
	list = make([]string, 0, len(imported))
	for k := range imported {
		list = append(list, k)
	}
	sort.Strings(list)
	data, _ := json.Marshal(list)
	return pdb.q.SetMemberImportedPorts(ctx, projectstore.SetMemberImportedPortsParams{ImportedPorts: string(data), Name: mrow.Name})
}

// httpPorts 是常见的网站端口（容器内）。9000（php-fpm 等）、443（容器内一般是 HTTPS）不算。
var httpPorts = map[uint32]bool{80: true, 8000: true, 8080: true, 8081: true, 8888: true, 3000: true, 3001: true,
	4000: true, 5000: true, 5173: true, 8096: true}

// likelyHTTP 判断容器端口是不是网站。导入 compose 端口时用：网站按 HTTP 开放，可以统计请求、绑定域名。
func likelyHTTP(target uint32) bool { return httpPorts[target] }

// routeLabel 用于日志，例如 web:80 → example.com、umami/db:5432 → tcp/15432。
func routeLabel(r *nagiv1.Exposure) string {
	target := fmt.Sprintf("%s:%d", r.GetMember(), r.GetTarget())
	if r.GetService() != r.GetMember() {
		target = fmt.Sprintf("%s/%s:%d", r.GetMember(), r.GetService(), r.GetTarget())
	}
	if len(r.GetDomains()) > 0 {
		return target + " → " + strings.Join(r.GetDomains(), ", ")
	}
	return fmt.Sprintf("%s → %s/%d", target, r.GetProtocol(), r.GetPublished())
}
