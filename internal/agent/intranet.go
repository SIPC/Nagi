package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 项目内网的跨节点部分（见 docs/agent.md#项目内网）：
//   - 面板算好每个节点的代理计划（LinkPlan）下发过来，节点缓存在 <节点目录>/links.pb，面板不在时照旧；
//   - 计划里的每个代理在本节点的项目内网里放一个同名的代理容器（nagi link-proxy），
//     代理把连接经 unix socket 交给节点，节点按计划里的顺序经 mesh 的 7001 端口交给目标所在的节点；
//   - 对方节点检查调用方有权访问这个项目，再连到项目里的那个容器，然后双向转发。容器不在它那里时回"不在这里"，
//     本节点接着找下一个节点（例如容器被顶替到了热备）。
//   - UDP 的部分见 intranet_udp.go。

// linkPort 是 mesh 上转发容器连接的 TCP 端口。
const linkPort = 7001

const (
	labelLink      = "nagi.link"
	labelLinkAlias = "nagi.link.alias"
	labelLinkPorts = "nagi.link.ports"
	labelLinkUDP   = "nagi.link.udp-ports"
	labelLinkBuild = "nagi.link.build"
)

type intranet struct {
	a    *agent
	kick chan struct{}
	file string

	mu     sync.Mutex
	plan   *nagiv1.LinkPlan
	socks  map[string]net.Listener // 项目 ID → 代理连进来的 unix socket（TCP 连接）
	usocks map[string]net.Listener // 项目 ID → 代理连进来的 unixpacket socket（UDP）
	build  string                  // 代理容器标签：nagi 换了版本就重建
	// endpoint 找本节点上项目内网里的容器（project.Manager.FindEndpoint），测试时替换。
	endpoint func(ctx context.Context, projectID, name string) (project.Endpoint, bool)
}

func newIntranet(a *agent) *intranet {
	n := &intranet{a: a, kick: make(chan struct{}, 1), file: filepath.Join(a.cfg.DataDir, "links.pb"),
		plan: &nagiv1.LinkPlan{}, socks: map[string]net.Listener{}, usocks: map[string]net.Listener{}}
	n.endpoint = a.projects.FindEndpoint
	n.build = a.cfg.Version
	if exe, err := linkExe(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			n.build += "-" + strconv.FormatInt(fi.ModTime().Unix(), 36)
		}
	}
	if b, err := os.ReadFile(n.file); err == nil {
		p := &nagiv1.LinkPlan{}
		if proto.Unmarshal(b, p) == nil {
			n.plan = p
		}
	}
	return n
}

func linkExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// set 保存面板下发的计划，立即调整代理。
func (n *intranet) set(p *nagiv1.LinkPlan) error {
	b, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.WriteFile(n.file+".tmp", b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(n.file+".tmp", n.file); err != nil {
		return err
	}
	n.mu.Lock()
	n.plan = p
	n.mu.Unlock()
	n.trigger()
	return nil
}

func (n *intranet) current() *nagiv1.LinkPlan {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.plan
}

// trigger 让 run 立即调整一次。
func (n *intranet) trigger() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// run 维持代理容器，直到 ctx 取消：计划变了、容器有变化时调整，另外每 30 秒检查一次。
func (n *intranet) run(ctx context.Context) {
	go n.serve(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		n.syncSockets()
		if err := n.reconcile(ctx); err != nil && ctx.Err() == nil {
			n.a.log.Warn("调整项目内网的代理容器失败", "err", err)
		}
		select {
		case <-ctx.Done():
			n.mu.Lock()
			for _, s := range n.socks {
				s.Close()
			}
			for _, s := range n.usocks {
				s.Close()
			}
			n.mu.Unlock()
			return
		case <-t.C:
		case <-n.kick:
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func linkContainerName(projectID, alias string) string { return "nagi-link-" + projectID + "-" + alias }

func portsLabel(ports []uint32) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(int(p))
	}
	return strings.Join(s, ",")
}

// 计划里每个项目最多的代理数、每个代理最多的端口数。
const (
	maxProxies     = 512
	maxProxyPorts  = 64
	maxProxyTarget = 8
)

// validAlias 检查代理在项目内网里的名字：容器、服务.容器组，连接其他项目时再加一段网络名。
func validAlias(alias string) bool {
	parts := strings.Split(alias, ".")
	if len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if !spec.NameRe.MatchString(p) {
			return false
		}
	}
	return true
}

// wanted 返回计划里要放的代理，键是代理容器名。本节点上没有容器的项目不放（项目内网不在本节点上）。
func (n *intranet) wanted(ctx context.Context) map[string]*nagiv1.LinkProxy {
	local := map[string]bool{}
	here := map[string]bool{} // 本节点上的容器（项目 ID/容器名）
	for _, c := range n.a.projects.List(ctx) {
		local[c.Spec.GetProjectId()] = true
		here[c.Spec.GetProjectId()+"/"+c.Spec.GetName()] = true
	}
	out := map[string]*nagiv1.LinkProxy{}
	for _, p := range n.current().GetProxies() {
		if len(out) >= maxProxies || !local[p.GetProjectId()] || !validAlias(p.GetAlias()) || (len(p.GetPorts()) == 0 && len(p.GetUdpPorts()) == 0) {
			continue
		}
		// 同项目的容器已经在本节点上（例如刚顶替过来、面板还没更新计划）：名字由容器自己占着，不放代理。
		if p.GetTargetProject() == p.GetProjectId() {
			parts := strings.Split(p.GetTargetName(), ".")
			if here[p.GetProjectId()+"/"+parts[len(parts)-1]] {
				continue
			}
		}
		out[linkContainerName(p.GetProjectId(), p.GetAlias())] = p
	}
	return out
}

// proxyPorts 整理代理的端口（TCP 或 UDP 的）：去掉不对的和重复的，最多 maxProxyPorts 个。
func proxyPorts(ports []uint32) []uint32 {
	var out []uint32
	for _, x := range ports {
		if x > 0 && x <= 65535 && len(out) < maxProxyPorts && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

// reconcile 让代理容器和计划一致：多的删掉，缺的建上，端口变了或者 nagi 升级了就重建。
func (n *intranet) reconcile(ctx context.Context) error {
	want := n.wanted(ctx)
	existing, err := n.a.docker.ManagedInstances(ctx, labelLink)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	var firstErr error
	for _, c := range existing {
		p, ok := want[c.Name]
		same := ok && c.Labels[labelLinkPorts] == portsLabel(proxyPorts(p.GetPorts())) &&
			c.Labels[labelLinkUDP] == portsLabel(proxyPorts(p.GetUdpPorts())) && c.Labels[labelLinkBuild] == n.build &&
			c.State != "exited" && c.State != "dead" && c.State != "created"
		if same {
			have[c.Name] = true
			continue
		}
		if err := n.a.docker.RemoveContainer(ctx, c.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(want) == len(have) {
		return firstErr
	}
	exe, err := linkExe()
	if err != nil {
		return err
	}
	if err := n.a.docker.EnsureLinkImage(ctx); err != nil {
		return fmt.Errorf("准备代理容器的镜像: %w", err)
	}
	for name, p := range want {
		if have[name] {
			continue
		}
		if err := n.a.projects.EnsureProjectNetwork(ctx, p.GetProjectId()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ports, udpPorts := portsLabel(proxyPorts(p.GetPorts())), portsLabel(proxyPorts(p.GetUdpPorts()))
		err := n.a.docker.RunLinkProxy(ctx, docker.LinkProxySpec{
			Name: name, Network: project.ProjectNetwork(p.GetProjectId()), Alias: p.GetAlias(),
			Exe: exe, SockDir: n.sockDir(p.GetProjectId()),
			Env: []string{"NAGI_LINK_ALIAS=" + p.GetAlias(), "NAGI_LINK_PORTS=" + ports, "NAGI_LINK_UDP_PORTS=" + udpPorts},
			Labels: map[string]string{labelLink: p.GetProjectId(), labelLinkAlias: p.GetAlias(), labelLinkPorts: ports,
				labelLinkUDP: udpPorts, labelLinkBuild: n.build},
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("创建 %s 的代理容器: %w", p.GetAlias(), err)
		}
	}
	return firstErr
}

// ---- 本节点这一侧：代理容器经 unix socket 连进来 ----

func (n *intranet) sockDir(projectID string) string {
	return filepath.Join(n.a.cfg.DataDir, "run", "link-"+projectID)
}

// syncSockets 为每个要放代理的项目开一个 unix socket，挂进这个项目的代理容器；有 UDP 端口的项目再开一个 unixpacket socket。
func (n *intranet) syncSockets() {
	want, wantUDP := map[string]bool{}, map[string]bool{}
	for _, p := range n.current().GetProxies() {
		want[p.GetProjectId()] = true
		if len(p.GetUdpPorts()) > 0 {
			wantUDP[p.GetProjectId()] = true
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.syncListeners(n.socks, want, "unix", "sock", n.handleLocal)
	n.syncListeners(n.usocks, wantUDP, "unixpacket", "udp", n.handleLocalUDP)
}

// syncListeners 让 have 里的 socket 和 want 一致。调用方持有 n.mu。
func (n *intranet) syncListeners(have map[string]net.Listener, want map[string]bool, network, file string, handle func(string, net.Conn)) {
	for id, s := range have {
		if !want[id] {
			s.Close()
			delete(have, id)
		}
	}
	for id := range want {
		if have[id] != nil || len(id) != 16 {
			continue
		}
		dir := n.sockDir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			n.a.log.Warn("创建代理的 socket 目录失败", "err", err)
			continue
		}
		path := filepath.Join(dir, file)
		_ = os.Remove(path)
		ln, err := net.Listen(network, path)
		if err != nil {
			n.a.log.Warn("监听代理的 socket 失败", "project", id, "socket", file, "err", err)
			continue
		}
		have[id] = ln
		go func(id string) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go handle(id, c)
			}
		}(id)
	}
}

// proxyFor 找计划里项目 projectID 的项目内网里名字是 alias 的代理。
func (n *intranet) proxyFor(projectID, alias string) *nagiv1.LinkProxy {
	for _, p := range n.current().GetProxies() {
		if p.GetProjectId() == projectID && p.GetAlias() == alias {
			return p
		}
	}
	return nil
}

// handleLocal 处理代理容器交过来的一个连接：第一行是"名字 端口"。按计划里的顺序找目标所在的节点。
func (n *intranet) handleLocal(projectID string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	alias, port, err := readLinkHeader(br)
	if err != nil {
		return
	}
	p := n.proxyFor(projectID, alias)
	if p == nil || !slices.Contains(p.GetPorts(), uint32(port)) {
		return
	}
	targets := p.GetNodes()
	if len(targets) > maxProxyTarget {
		targets = targets[:maxProxyTarget]
	}
	for _, t := range targets {
		k, err := mesh.ParseKey(t.GetKey())
		if err != nil {
			continue
		}
		if n.a.mesh != nil && k == n.a.mesh.m.PublicKey() {
			// 目标就在本节点上（例如连接了同一个节点上的另一个项目）。
			e, ok := n.endpoint(n.a.ctx, p.GetTargetProject(), p.GetTargetName())
			if !ok {
				continue
			}
			tc, err := net.DialTimeout("tcp", netip.AddrPortFrom(e.IP, port).String(), 5*time.Second)
			if err != nil {
				continue
			}
			splice(c, br, tc, tc)
			tc.Close()
			return
		}
		mc, mbr, ok := n.dialRemote(k, t.GetName(), p, port)
		if !ok {
			continue
		}
		splice(c, br, mc, mbr)
		mc.Close()
		return
	}
}

// dialRemote 经 mesh 请目标节点连到它上面的容器。对方说"不在这里"或者联系不上时返回 false。
func (n *intranet) dialRemote(k mesh.Key, name string, p *nagiv1.LinkProxy, port uint16) (net.Conn, *bufio.Reader, bool) {
	if n.a.mesh == nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(n.a.ctx, 10*time.Second)
	mc, err := n.a.mesh.m.Dial(ctx, k, linkPort)
	cancel()
	if err != nil {
		n.a.log.Debug("经 mesh 连接目标节点失败", "name", p.GetAlias(), "node", name, "err", err)
		return nil, nil, false
	}
	if _, err := fmt.Fprintf(mc, "%s %s %d\n", p.GetTargetProject(), p.GetTargetName(), port); err != nil {
		mc.Close()
		return nil, nil, false
	}
	mbr := bufio.NewReader(mc)
	_ = mc.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(mbr)
	if err != nil || line != "OK" {
		n.a.log.Debug("目标节点没有接受连接", "name", p.GetAlias(), "node", name, "reply", line, "err", err)
		mc.Close()
		return nil, nil, false
	}
	_ = mc.SetReadDeadline(time.Time{})
	return mc, mbr, true
}

// ---- 目标这一侧：经 mesh 连进来 ----

// serve 在 mesh 的 7001 端口接受其他节点的连接。
func (n *intranet) serve(ctx context.Context) {
	if n.a.mesh == nil {
		return
	}
	ln, err := n.a.mesh.m.Listen(linkPort)
	if err != nil {
		n.a.log.Warn("mesh 上监听项目内网的连接失败", "err", err)
		return
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go n.handleRemote(ctx, c)
	}
}

// allowed 返回节点 k 能不能访问本节点上项目 projectID 的容器，以及项目设置里补的内部端口。
func (n *intranet) allowed(projectID string, k mesh.Key) (bool, []string) {
	for _, a := range n.current().GetAllows() {
		if a.GetProjectId() != projectID {
			continue
		}
		for _, c := range a.GetCallers() {
			if bytes.Equal(c, k[:]) {
				return true, a.GetInternalPorts()
			}
		}
	}
	return false, nil
}

// handleRemote 处理其他节点经 mesh 连进来的一个连接：第一行是"项目 名字 端口"。
func (n *intranet) handleRemote(ctx context.Context, c net.Conn) {
	defer c.Close()
	reply := func(msg string) { fmt.Fprintf(c, "%s\n", msg) }
	caller, ok := n.a.mesh.m.KeyOf(c.RemoteAddr())
	if !ok {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	line, err := readLine(br)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if rest, ok := strings.CutPrefix(line, "udp "); ok {
		n.handleRemoteUDP(ctx, c, br, caller, rest)
		return
	}
	projectID, rest, _ := strings.Cut(line, " ")
	name, port, err := parseLinkHeader(rest)
	if err != nil {
		reply("ERR 请求格式不对")
		return
	}
	ok, internal := n.allowed(projectID, caller)
	if !ok {
		reply("ERR 无权访问这个项目")
		return
	}
	e, found := n.endpoint(ctx, projectID, name)
	if !found {
		reply("ERR 不在这里")
		return
	}
	// 只转发开放的端口（容器声明的和项目设置里补的内部端口）：调用方节点不一定可信，不能借此访问容器的其他端口。
	open := slices.Contains(e.Ports, uint32(port)) || slices.Contains(spec.InternalPortMap(internal)[name], uint32(port))
	if !open {
		reply(fmt.Sprintf("ERR %s 没有开放端口 %d", name, port))
		return
	}
	tc, err := net.DialTimeout("tcp", netip.AddrPortFrom(e.IP, port).String(), 5*time.Second)
	if err != nil {
		reply("ERR " + err.Error())
		return
	}
	defer tc.Close()
	if _, err := io.WriteString(c, "OK\n"); err != nil {
		return
	}
	splice(c, br, tc, tc)
}

// ---- 公共部分 ----

func readLine(br *bufio.Reader) (string, error) {
	var b []byte
	for len(b) < 512 {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			return string(b), nil
		}
		b = append(b, c)
	}
	return "", errors.New("请求太长")
}

// readLinkHeader 读"名字 端口"一行。
func readLinkHeader(br *bufio.Reader) (string, uint16, error) {
	line, err := readLine(br)
	if err != nil {
		return "", 0, err
	}
	return parseLinkHeader(line)
}

func parseLinkHeader(s string) (string, uint16, error) {
	alias, p, ok := strings.Cut(s, " ")
	num, err := strconv.ParseUint(p, 10, 16)
	if !ok || alias == "" || err != nil || num == 0 {
		return "", 0, errors.New("请求格式不对")
	}
	return alias, uint16(num), nil
}
