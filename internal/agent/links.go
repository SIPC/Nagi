package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 跨节点连接共享网络（见 docs/agent.md#跨节点连接共享网络）：
//   - 每 10 秒经 mesh 问一遍相连的节点"你这里的网络里有哪些成员、开着哪些端口"（SharedNetworkInfo）；
//   - 对方的每个成员在本节点的网络里有一个同名的代理容器（nagi link-proxy），
//     代理把连接经 unix socket 交给节点，节点经 mesh 的 7001 端口交给对方节点；
//   - 对方节点检查调用方和这个网络相连，再连到网络里的那个容器，然后双向转发。
// 代理在每次连接时才查名字现在在哪个节点，所以项目顶替、迁移到另一个相连的节点后会自动跟过去。

// linkPort 是 mesh 上转发容器连接的 TCP 端口。
const linkPort = 7001

const (
	labelLink      = "nagi.link"
	labelLinkAlias = "nagi.link.alias"
	labelLinkPorts = "nagi.link.ports"
	labelLinkBuild = "nagi.link.build"
	// 对方成员所在的节点，节点重启后先按这些标签恢复成员列表，免得联系上对方之前把代理都删了。
	labelLinkNode     = "nagi.link.node"
	labelLinkNodeName = "nagi.link.node-name"
	labelLinkProject  = "nagi.link.project"
)

type linkPeerState struct {
	name, nodeID string
	ok           bool
	err          string
	at           time.Time
	endpoints    []*nagiv1.LinkEndpoint
}

type links struct {
	a      *agent
	kick   chan struct{}
	syncMu sync.Mutex // 同一时间只有一次同步（定时的、面板改了连接后立即做的）
	seeded bool       // 已经按现有的代理容器恢复过成员列表

	mu      sync.Mutex
	linked  map[string][]project.LinkPeer          // 网络 → 相连的节点（数据库的缓存）
	peers   map[string]map[mesh.Key]*linkPeerState // 网络 → 相连节点上次的回答
	local   map[string][]project.LinkEndpoint      // 网络 → 本节点的成员（缓存）
	localAt map[string]time.Time                   // 网络 → 上次读取本节点成员的时间
	socks   map[string]net.Listener                // 网络 → 代理连进来的 unix socket
	warned  map[string]bool                        // 已经告警过的撞名
	build   string                                 // 代理容器标签：nagi 换了版本就重建
}

func newLinks(a *agent) *links {
	l := &links{
		a: a, kick: make(chan struct{}, 1),
		linked: map[string][]project.LinkPeer{}, peers: map[string]map[mesh.Key]*linkPeerState{},
		local: map[string][]project.LinkEndpoint{}, localAt: map[string]time.Time{},
		socks: map[string]net.Listener{}, warned: map[string]bool{},
	}
	l.build = a.cfg.Version
	if exe, err := linkExe(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			l.build += "-" + strconv.FormatInt(fi.ModTime().Unix(), 36)
		}
	}
	return l
}

func linkExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// trigger 让 run 立即同步一次。
func (l *links) trigger() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// run 维持代理容器和成员列表，直到 ctx 取消。
func (l *links) run(ctx context.Context) {
	go l.serve(ctx)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		l.sync(ctx)
		select {
		case <-ctx.Done():
			l.mu.Lock()
			for _, s := range l.socks {
				s.Close()
			}
			l.mu.Unlock()
			return
		case <-t.C:
		case <-l.kick:
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (l *links) sync(ctx context.Context) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	if !l.seeded {
		l.seed(ctx)
	}
	linked, err := l.a.projects.SharedNetworkLinks(ctx)
	if err != nil {
		l.a.log.Warn("读取共享网络的连接失败", "err", err)
		return
	}
	l.mu.Lock()
	l.linked = linked
	l.mu.Unlock()

	// 问每个相连的节点（并行）。
	type answer struct {
		network string
		key     mesh.Key
		peer    project.LinkPeer
		resp    *nagiv1.SharedNetworkInfoResponse
		err     error
	}
	var wg sync.WaitGroup
	ch := make(chan answer, 16)
	for network, peers := range linked {
		l.refreshLocal(ctx, network)
		for _, p := range peers {
			k, err := mesh.ParseKey(p.Key)
			if err != nil {
				continue
			}
			wg.Add(1)
			go func(network string, k mesh.Key, p project.LinkPeer) {
				defer wg.Done()
				resp, err := l.query(ctx, k, network)
				ch <- answer{network, k, p, resp, err}
			}(network, k, p)
		}
	}
	go func() { wg.Wait(); close(ch) }()
	got := map[string]map[mesh.Key]answer{}
	for a := range ch {
		if got[a.network] == nil {
			got[a.network] = map[mesh.Key]answer{}
		}
		got[a.network][a.key] = a
	}

	l.mu.Lock()
	next := map[string]map[mesh.Key]*linkPeerState{}
	for network, answers := range got {
		next[network] = map[mesh.Key]*linkPeerState{}
		for k, a := range answers {
			st := &linkPeerState{name: a.peer.Name, nodeID: a.peer.NodeID, at: time.Now()}
			if a.err == nil {
				st.ok, st.endpoints = true, a.resp.GetEndpoints()
			} else {
				st.err = linkError(a.err)
				// 暂时联系不上：保留上次的成员，代理容器不动（连接会失败），恢复后照常。
				// 对方明确说没有相连（断开了连接）时删掉它的成员。
				if old := l.peers[network][k]; old != nil && status.Code(a.err) != codes.PermissionDenied {
					st.endpoints = old.endpoints
				}
			}
			next[network][k] = st
		}
	}
	l.peers = next
	l.mu.Unlock()

	l.syncSockets(linked)
	if err := l.reconcile(ctx); err != nil && ctx.Err() == nil {
		l.a.log.Warn("调整共享网络的代理容器失败", "err", err)
	}
}

// seed 节点刚启动时按现有的代理容器恢复对方的成员列表：联系上对方之前代理照常工作，不会被删掉再重建。
func (l *links) seed(ctx context.Context) {
	cs, err := l.a.docker.ManagedContainers(ctx, labelLink)
	if err != nil {
		return
	}
	l.seeded = true
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range cs {
		raw, err := hex.DecodeString(c.Labels[labelLinkNode])
		if err != nil {
			continue
		}
		k, err := mesh.ParseKey(raw)
		if err != nil {
			continue
		}
		network := c.Labels[labelLink]
		if l.peers[network] == nil {
			l.peers[network] = map[mesh.Key]*linkPeerState{}
		}
		st := l.peers[network][k]
		if st == nil {
			st = &linkPeerState{name: c.Labels[labelLinkNodeName], err: "节点刚启动，还没有联系上"}
			l.peers[network][k] = st
		}
		var ports []uint32
		for _, s := range strings.Split(c.Labels[labelLinkPorts], ",") {
			if n, err := strconv.ParseUint(s, 10, 16); err == nil {
				ports = append(ports, uint32(n))
			}
		}
		st.endpoints = append(st.endpoints, &nagiv1.LinkEndpoint{
			Alias: c.Labels[labelLinkAlias], ProjectName: c.Labels[labelLinkProject], Ports: ports,
		})
	}
}

func linkError(err error) string {
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.PermissionDenied:
			return "对方没有和本节点相连：" + s.Message()
		case codes.Unavailable, codes.DeadlineExceeded:
			return "经 mesh 联系不上"
		}
		return s.Message()
	}
	return err.Error()
}

// query 经 mesh 问一个节点它的网络 network 里有哪些成员。
func (l *links) query(ctx context.Context, k mesh.Key, network string) (*nagiv1.SharedNetworkInfoResponse, error) {
	if l.a.mesh == nil {
		return nil, errors.New("mesh 没有启动")
	}
	cc, err := mesh.Client(l.a.mesh.m, k)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return nagiv1.NewMeshServiceClient(cc).SharedNetworkInfo(ctx, &nagiv1.SharedNetworkInfoRequest{Name: network})
}

// refreshLocal 重新读取本节点网络 network 里的成员。
func (l *links) refreshLocal(ctx context.Context, network string) []project.LinkEndpoint {
	eps, err := l.a.projects.LinkEndpoints(ctx, network)
	if err != nil {
		l.mu.Lock()
		eps = l.local[network]
		l.mu.Unlock()
		return eps
	}
	l.mu.Lock()
	l.local[network], l.localAt[network] = eps, time.Now()
	l.mu.Unlock()
	return eps
}

// localEndpoint 找本节点网络 network 里名字是 alias 的成员。缓存里没有时（最多每秒一次）重新读取。
func (l *links) localEndpoint(ctx context.Context, network, alias string) (project.LinkEndpoint, bool) {
	find := func(eps []project.LinkEndpoint) (project.LinkEndpoint, bool) {
		for _, e := range eps {
			if e.Alias == alias {
				return e, true
			}
		}
		return project.LinkEndpoint{}, false
	}
	l.mu.Lock()
	eps, at := l.local[network], l.localAt[network]
	l.mu.Unlock()
	if e, ok := find(eps); ok {
		return e, true
	}
	if time.Since(at) < time.Second {
		return project.LinkEndpoint{}, false
	}
	return find(l.refreshLocal(ctx, network))
}

// isLinked 判断节点 k 是否和本节点的网络 network 相连。缓存里没有时查一次数据库（刚连上还没同步）。
func (l *links) isLinked(ctx context.Context, network string, k mesh.Key) bool {
	has := func(m map[string][]project.LinkPeer) bool {
		for _, p := range m[network] {
			if bytes.Equal(p.Key, k[:]) {
				return true
			}
		}
		return false
	}
	l.mu.Lock()
	ok := has(l.linked)
	l.mu.Unlock()
	if ok {
		return true
	}
	m, err := l.a.projects.SharedNetworkLinks(ctx)
	return err == nil && has(m)
}

// remoteEndpoint 是代理容器要代表的一个对方成员。
type remoteEndpoint struct {
	network, alias string
	node           mesh.Key
	nodeName       string
	projectName    string
	ports          []uint32
}

// remotes 返回各网络里对方节点上的成员：本节点也有的名字不算；几个节点都有时按节点公钥顺序取第一个并告警。
func (l *links) remotes() map[string]map[string]remoteEndpoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]map[string]remoteEndpoint{}
	for network, peers := range l.peers {
		local := map[string]bool{}
		for _, e := range l.local[network] {
			local[e.Alias] = true
		}
		keys := make([]mesh.Key, 0, len(peers))
		for k := range peers {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
		m := map[string]remoteEndpoint{}
		for _, k := range keys {
			st := peers[k]
			for _, e := range st.endpoints {
				alias := e.GetAlias()
				conflict := ""
				if local[alias] {
					conflict = "本节点"
				} else if prev, ok := m[alias]; ok {
					conflict = prev.nodeName
				}
				if conflict != "" {
					w := network + "/" + alias + "/" + st.name
					if !l.warned[w] {
						l.warned[w] = true
						l.a.log.Warn("共享网络里的名字重复，用先找到的那个", "network", network, "name", alias, "node", st.name, "kept", conflict)
					}
					continue
				}
				m[alias] = remoteEndpoint{
					network: network, alias: alias, node: k, nodeName: st.name,
					projectName: e.GetProjectName(), ports: e.GetPorts(),
				}
			}
		}
		out[network] = m
	}
	return out
}

func linkContainerName(network, alias string) string { return "nagi-link-" + network + "-" + alias }

func portsLabel(ports []uint32) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(int(p))
	}
	return strings.Join(s, ",")
}

// reconcile 让代理容器和对方的成员一致：多的删掉，缺的建上，端口变了或者 nagi 升级了就重建。
func (l *links) reconcile(ctx context.Context) error {
	want := map[string]remoteEndpoint{}
	for _, m := range l.remotes() {
		for _, e := range m {
			want[linkContainerName(e.network, e.alias)] = e
		}
	}
	existing, err := l.a.docker.ManagedContainers(ctx, labelLink)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	var firstErr error
	for _, c := range existing {
		e, ok := want[c.Name]
		same := ok && c.Labels[labelLinkPorts] == portsLabel(e.ports) && c.Labels[labelLinkBuild] == l.build &&
			c.State != "exited" && c.State != "dead" && c.State != "created"
		if same {
			have[c.Name] = true
			continue
		}
		if err := l.a.docker.RemoveContainer(ctx, c.ID); err != nil && firstErr == nil {
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
	if err := l.a.docker.EnsureLinkImage(ctx); err != nil {
		return fmt.Errorf("准备代理容器的镜像: %w", err)
	}
	for name, e := range want {
		if have[name] {
			continue
		}
		err := l.a.docker.RunLinkProxy(ctx, docker.LinkProxySpec{
			Name: name, Network: project.SharedNetworkDocker(e.network), Alias: e.alias,
			Exe: exe, SockDir: l.sockDir(e.network),
			Env: []string{"NAGI_LINK_ALIAS=" + e.alias, "NAGI_LINK_PORTS=" + portsLabel(e.ports)},
			Labels: map[string]string{
				labelLink: e.network, labelLinkAlias: e.alias, labelLinkPorts: portsLabel(e.ports), labelLinkBuild: l.build,
				labelLinkNode: hex.EncodeToString(e.node[:]), labelLinkNodeName: e.nodeName, labelLinkProject: e.projectName,
			},
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("创建 %s 的代理容器: %w", e.alias, err)
		}
	}
	return firstErr
}

// ---- 本节点这一侧：代理容器经 unix socket 连进来 ----

func (l *links) sockDir(network string) string {
	return filepath.Join(l.a.cfg.DataDir, "run", "link-"+network)
}

// syncSockets 为每个相连的网络开一个 unix socket，挂进这个网络的代理容器。
func (l *links) syncSockets(linked map[string][]project.LinkPeer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for network, s := range l.socks {
		if len(linked[network]) == 0 {
			s.Close()
			delete(l.socks, network)
		}
	}
	for network, peers := range linked {
		if len(peers) == 0 || l.socks[network] != nil {
			continue
		}
		dir := l.sockDir(network)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			l.a.log.Warn("创建代理的 socket 目录失败", "err", err)
			continue
		}
		path := filepath.Join(dir, "sock")
		_ = os.Remove(path)
		ln, err := net.Listen("unix", path)
		if err != nil {
			l.a.log.Warn("监听代理的 socket 失败", "network", network, "err", err)
			continue
		}
		l.socks[network] = ln
		go func(network string) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go l.handleLocal(network, c)
			}
		}(network)
	}
}

// handleLocal 处理代理容器交过来的一个连接：第一行是"名字 端口"，经 mesh 交给名字现在所在的节点。
func (l *links) handleLocal(network string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	alias, port, err := readLinkHeader(br)
	if err != nil {
		return
	}
	e, ok := l.remotes()[network][alias]
	if !ok || l.a.mesh == nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.a.ctx, 10*time.Second)
	mc, err := l.a.mesh.m.Dial(ctx, e.node, linkPort)
	cancel()
	if err != nil {
		l.a.log.Debug("经 mesh 连接对方节点失败", "network", network, "name", alias, "node", e.nodeName, "err", err)
		return
	}
	defer mc.Close()
	if _, err := fmt.Fprintf(mc, "%s %s %d\n", network, alias, port); err != nil {
		return
	}
	mbr := bufio.NewReader(mc)
	_ = mc.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(mbr)
	if err != nil || line != "OK" {
		l.a.log.Debug("对方节点拒绝了连接", "network", network, "name", alias, "reply", line, "err", err)
		return
	}
	_ = mc.SetReadDeadline(time.Time{})
	splice(c, br, mc, mbr)
}

// ---- 对方这一侧：经 mesh 连进来 ----

// serve 在 mesh 的 7001 端口接受相连节点的连接。
func (l *links) serve(ctx context.Context) {
	if l.a.mesh == nil {
		return
	}
	ln, err := l.a.mesh.m.Listen(linkPort)
	if err != nil {
		l.a.log.Warn("mesh 上监听共享网络的连接失败", "err", err)
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
		go l.handleRemote(ctx, c)
	}
}

// handleRemote 处理相连节点经 mesh 连进来的一个连接：第一行是"网络 名字 端口"。
func (l *links) handleRemote(ctx context.Context, c net.Conn) {
	defer c.Close()
	reject := func(msg string) { fmt.Fprintf(c, "ERR %s\n", msg) }
	caller, ok := l.a.mesh.m.KeyOf(c.RemoteAddr())
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
	network, rest, _ := strings.Cut(line, " ")
	alias, port, err := parseLinkHeader(rest)
	if err != nil {
		reject("请求格式不对")
		return
	}
	if !l.isLinked(ctx, network, caller) {
		reject("没有和本节点的这个网络相连")
		return
	}
	e, ok := l.localEndpoint(ctx, network, alias)
	if !ok {
		reject("网络里没有 " + alias)
		return
	}
	tc, err := net.DialTimeout("tcp", netip.AddrPortFrom(e.IP, port).String(), 5*time.Second)
	if err != nil {
		reject(err.Error())
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
	n, err := strconv.ParseUint(p, 10, 16)
	if !ok || alias == "" || err != nil || n == 0 {
		return "", 0, errors.New("请求格式不对")
	}
	return alias, uint16(n), nil
}

// splice 在 a、b 之间双向转发，ar、br 是两边的读取端（可能带着已经读进缓冲的数据）。
// 一边读完以后关闭另一边的写方向，两边都结束后返回。
func splice(a net.Conn, ar io.Reader, b net.Conn, br io.Reader) {
	done := make(chan struct{}, 2)
	pipe := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(b, ar)
	go pipe(a, br)
	<-done
	<-done
}

// ---- 接口 ----

// decorate 给节点的共享网络列表补上相连节点的状态和对方的成员。
func (l *links) decorate(list []*nagiv1.SharedNetwork) {
	remotes := l.remotes()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range list {
		for _, p := range n.GetPeers() {
			k, err := mesh.ParseKey(p.GetPublicKey())
			if err != nil {
				continue
			}
			if st := l.peers[n.GetName()][k]; st != nil {
				p.Reachable, p.Error = st.ok, st.err
			} else {
				p.Error = "还没有联系过"
			}
		}
		for _, e := range remotes[n.GetName()] {
			n.Remotes = append(n.Remotes, &nagiv1.SharedNetworkRemote{
				Alias: e.alias, NodeName: e.nodeName, ProjectName: e.projectName, Ports: e.ports,
			})
		}
		sort.Slice(n.Remotes, func(i, j int) bool { return n.Remotes[i].Alias < n.Remotes[j].Alias })
	}
}

// remoteProjects 问每个相连的节点：它那里加入了网络 network 的项目（检查网络名是否重复）。
func (l *links) remoteProjects(ctx context.Context, network string) ([]*nagiv1.SharedNetworkProject, error) {
	linked, err := l.a.projects.SharedNetworkLinks(ctx)
	if err != nil {
		return nil, err
	}
	var out []*nagiv1.SharedNetworkProject
	var failed []string
	for _, p := range linked[network] {
		k, err := mesh.ParseKey(p.Key)
		if err != nil {
			continue
		}
		resp, err := l.query(ctx, k, network)
		if err != nil {
			failed = append(failed, p.Name)
			continue
		}
		out = append(out, resp.GetProjects()...)
	}
	if len(failed) > 0 {
		return out, fmt.Errorf("联系不上相连的节点 %s", strings.Join(failed, "、"))
	}
	return out, nil
}

// SharedNetworkInfo 回答相连节点的查询：本节点这个网络里的成员和加入的项目。
func (s *meshService) SharedNetworkInfo(ctx context.Context, req *nagiv1.SharedNetworkInfoRequest) (*nagiv1.SharedNetworkInfoResponse, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok || s.a.links == nil || !s.a.links.isLinked(ctx, req.GetName(), caller) {
		return nil, status.Error(codes.PermissionDenied, "没有和本节点的这个共享网络相连")
	}
	out := &nagiv1.SharedNetworkInfoResponse{Projects: s.a.projects.SharedNetworkProjects(ctx, req.GetName())}
	for _, e := range s.a.links.refreshLocal(ctx, req.GetName()) {
		out.Endpoints = append(out.Endpoints, &nagiv1.LinkEndpoint{
			Alias: e.Alias, ProjectId: e.ProjectID, ProjectName: e.ProjectName, Ports: e.Ports,
		})
	}
	return out, nil
}
