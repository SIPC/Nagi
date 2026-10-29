package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 项目内网的 UDP（见 docs/agent.md#项目内网）。数据报多路复用在一条通道上：
//   - 代理容器的每个 UDP 端口和节点之间开一条 unixpacket 连接（挂进去的 /run/nagi/udp），第一条消息是"名字 端口"，
//     之后每条消息是一个数据报，前面加 4 字节的流 ID（代理按客户端地址分配，不重复使用）；
//   - 节点按计划里的顺序经 mesh 的 7001 端口握手："udp 项目 名字 端口 本端的 UDP 端口"，对方和 TCP 一样检查权限后
//     回"OK 对方的 UDP 端口"。之后数据报原样（带流 ID）经 mesh 的 UDP 收发；
//   - 握手的 TCP 连接留着做这条通道的控制连接：两边定时发空行，断了（例如对方节点挂了）、目标容器不在了或换了地址时
//     拆掉通道，本节点重新握手（容器顶替以后就连到新节点）。本节点有数据要发时还会发 "?"（最多每秒一次），
//     对方立刻回空行，udpPokeTimeout 没回就拆掉通道：对方节点重启、断网时不用等心跳超时；
//   - 目标节点为每个流开一个 UDP socket 连到容器，所以容器看到的每个客户端是不同的来源端口；流空闲一段时间后关掉。

const (
	// udpMaxDatagram 是转发的单个数据报的最大长度，更长的丢掉。mesh 的 MTU 是 1280，再长的靠 IPv6 分片。
	udpMaxDatagram = 16 << 10
	udpBufSize     = 4 + udpMaxDatagram + 1 // 流 ID + 数据报，多 1 字节用来认出超长（被截断）的
	// udpMaxFlows 是一条通道上最多同时的流，满了关掉最久没动的。
	udpMaxFlows = 1024
	// 流的空闲时间：只发过一个数据报的（例如 DNS 查询）很快关掉，其余的久一点（和 Linux conntrack 的 UDP 超时一样）。
	udpShortIdle = 30 * time.Second
	udpIdle      = 2 * time.Minute
	// 控制连接每 udpHeartbeat 发一个空行，udpDeadAfter 没收到对方的就算断了。
	udpHeartbeat = 10 * time.Second
	udpDeadAfter = 30 * time.Second
	// udpPoke：有数据要发、而且这么久没问过时，在控制连接上问一下对方还在不在；udpPokeTimeout 没回就算断了。
	udpPoke        = time.Second
	udpPokeTimeout = 3 * time.Second
	// udpDial 是建通道时连接对方节点的超时。短一点：对方重启期间的连接尝试会卡在 SYN 重传上，不如快点重新连。
	udpDial = 3 * time.Second
	// udpRetry 是通道连续刚建好就断了时，重试前等的时间；udpRetryDial 是哪个节点都连不上时重试前等的时间。
	udpRetry     = 3 * time.Second
	udpRetryDial = time.Second
	// udpReconnect 是代理容器连不上节点（例如节点正在重启）时重连的间隔。
	udpReconnect = time.Second
)

// udpIdleFor 返回流的空闲超时，sent 是客户端发过的数据报数。
func udpIdleFor(sent int64) time.Duration {
	if sent > 1 {
		return udpIdle
	}
	return udpShortIdle
}

// udpTunnel 是一条 UDP 通道：send 发出一个带流 ID 的数据报，对方回的数据报交给建通道时给的 reply。
type udpTunnel interface {
	send(msg []byte)
	done() <-chan struct{}
	close()
}

// ---- 目标容器这一侧：按流 ID 分发 ----

// udpFlows 把带流 ID 的数据报分发给目标容器：每个流一个连到容器的 UDP socket，容器回的数据报加上流 ID 交给 reply。
type udpFlows struct {
	target netip.AddrPort
	reply  func([]byte)
	stop   chan struct{}

	mu     sync.Mutex
	flows  map[uint32]*udpFlow
	closed bool
}

type udpFlow struct {
	c    *net.UDPConn
	last atomic.Int64 // 最近一次收发的时间（UnixNano）
	sent atomic.Int64 // 客户端发过的数据报数，只关心是不是超过 1
}

func newUDPFlows(target netip.AddrPort, reply func([]byte)) *udpFlows {
	return &udpFlows{target: target, reply: reply, stop: make(chan struct{}), flows: map[uint32]*udpFlow{}}
}

func (f *udpFlows) done() <-chan struct{} { return f.stop }

func (f *udpFlows) send(msg []byte) {
	if len(msg) < 4 {
		return
	}
	id := binary.BigEndian.Uint32(msg)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	fl := f.flows[id]
	if fl == nil {
		if len(f.flows) >= udpMaxFlows {
			f.evictLocked()
		}
		c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(f.target))
		if err != nil {
			f.mu.Unlock()
			return
		}
		fl = &udpFlow{c: c}
		fl.last.Store(time.Now().UnixNano())
		f.flows[id] = fl
		go f.recv(id, fl)
	}
	f.mu.Unlock()
	fl.last.Store(time.Now().UnixNano())
	if fl.sent.Load() < 2 {
		fl.sent.Add(1)
	}
	_, _ = fl.c.Write(msg[4:])
}

// recv 把容器回的数据报交给 reply，直到流空闲超时或者被关掉。
func (f *udpFlows) recv(id uint32, fl *udpFlow) {
	defer f.drop(id, fl)
	buf := make([]byte, udpBufSize)
	binary.BigEndian.PutUint32(buf, id)
	for {
		deadline := time.Unix(0, fl.last.Load()).Add(udpIdleFor(fl.sent.Load()))
		if !time.Now().Before(deadline) {
			return
		}
		_ = fl.c.SetReadDeadline(deadline)
		n, err := fl.c.Read(buf[4:])
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue // 期间可能又发过数据，按最新的时间重新算
			}
			return
		}
		if n > udpMaxDatagram {
			continue
		}
		fl.last.Store(time.Now().UnixNano())
		f.reply(buf[:4+n])
	}
}

func (f *udpFlows) drop(id uint32, fl *udpFlow) {
	f.mu.Lock()
	if f.flows[id] == fl {
		delete(f.flows, id)
	}
	f.mu.Unlock()
	fl.c.Close()
}

// evictLocked 关掉最久没动的流。调用方持有 f.mu。
func (f *udpFlows) evictLocked() {
	var oldID uint32
	var old *udpFlow
	for id, fl := range f.flows {
		if old == nil || fl.last.Load() < old.last.Load() {
			oldID, old = id, fl
		}
	}
	if old != nil {
		delete(f.flows, oldID)
		old.c.Close()
	}
}

func (f *udpFlows) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for _, fl := range f.flows {
		fl.c.Close()
	}
	f.flows = nil
	close(f.stop)
}

// keepAlive 维持 UDP 通道的控制连接：每 udpHeartbeat 发一个空行，udpDeadAfter 没收到对方的就算断了；
// stop 关闭、check 返回 false 时也结束。收到的每个字节交给 onByte（可以为 nil）。返回后由调用方关闭连接。
func keepAlive(c net.Conn, br *bufio.Reader, stop <-chan struct{}, check func() bool, onByte func(byte)) {
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		for {
			_ = c.SetReadDeadline(time.Now().Add(udpDeadAfter))
			b, err := br.ReadByte()
			if err != nil {
				return
			}
			if onByte != nil {
				onByte(b)
			}
		}
	}()
	t := time.NewTicker(udpHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-dead:
			return
		case <-t.C:
		}
		_ = c.SetWriteDeadline(time.Now().Add(udpDeadAfter))
		if _, err := c.Write([]byte("\n")); err != nil {
			return
		}
		if check != nil && !check() {
			return
		}
	}
}

// ---- 本节点这一侧：代理容器经 unixpacket socket 连进来 ----

// handleLocalUDP 处理代理容器的一个 UDP 端口：第一条消息是"名字 端口"，之后每条消息是带流 ID 的数据报。
// 连接一直开着，期间按计划里的顺序维持到目标的通道，断了就重新建。
func (n *intranet) handleLocalUDP(projectID string, c net.Conn) {
	defer c.Close()
	buf := make([]byte, udpBufSize)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := c.Read(buf)
	if err != nil {
		return
	}
	alias, port, err := parseLinkHeader(strings.TrimSuffix(string(buf[:m]), "\n"))
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	reply := func(msg []byte) { _, _ = c.Write(msg) }

	var mu sync.Mutex
	var cur udpTunnel
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// 计划里没有这个代理（或者这个端口）了，或者节点退出：关掉连接，代理容器过一会儿重连。
		defer c.Close()
		wait := func(d time.Duration) bool {
			select {
			case <-stop:
				return false
			case <-n.a.ctx.Done():
				return false
			case <-time.After(d):
				return true
			}
		}
		short := 0 // 连续几条通道刚建好就断了
		for {
			p := n.proxyFor(projectID, alias)
			if p == nil || !slices.Contains(p.GetUdpPorts(), uint32(port)) {
				return
			}
			t := n.openUDP(p, port, reply)
			if t == nil {
				if !wait(udpRetryDial) {
					return
				}
				continue
			}
			start := time.Now()
			mu.Lock()
			cur = t
			mu.Unlock()
			select {
			case <-stop:
			case <-n.a.ctx.Done():
			case <-t.done():
			}
			mu.Lock()
			cur = nil
			mu.Unlock()
			t.close()
			// 第一次断了马上重连（例如对方节点刚重启）；连续刚建好就断（例如对方的容器一直在重建）就等一会儿，
			// 不要一直握手。
			if time.Since(start) < udpHeartbeat {
				short++
			} else {
				short = 0
			}
			var d time.Duration
			if short > 1 {
				d = udpRetry
			}
			if !wait(d) {
				return
			}
		}
	}()
	for {
		m, err := c.Read(buf)
		if err != nil {
			return
		}
		if m < 4 || m > 4+udpMaxDatagram {
			continue
		}
		mu.Lock()
		t := cur
		mu.Unlock()
		if t != nil {
			t.send(buf[:m])
		}
	}
}

// openUDP 按计划里的顺序找目标所在的节点，建一条 UDP 通道。都不行时返回 nil。
func (n *intranet) openUDP(p *nagiv1.LinkProxy, port uint16, reply func([]byte)) udpTunnel {
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
			if tun := n.localUDP(p, port, reply); tun != nil {
				return tun
			}
			continue
		}
		if tun := n.dialRemoteUDP(k, t.GetName(), p, port, reply); tun != nil {
			return tun
		}
	}
	return nil
}

// localUDP 是目标就在本节点上时的通道（例如连接了同一个节点上的另一个项目）：直接分发到容器。
// 容器不在了或者换了地址时结束，由调用方重新找。
func (n *intranet) localUDP(p *nagiv1.LinkProxy, port uint16, reply func([]byte)) udpTunnel {
	e, ok := n.endpoint(n.a.ctx, p.GetTargetProject(), p.GetTargetName())
	if !ok {
		return nil
	}
	f := newUDPFlows(netip.AddrPortFrom(e.IP, port), reply)
	go func() {
		t := time.NewTicker(udpHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-f.done():
				return
			case <-t.C:
			}
			if cur, ok := n.endpoint(n.a.ctx, p.GetTargetProject(), p.GetTargetName()); !ok || cur.IP != e.IP {
				f.close()
				return
			}
		}
	}()
	return f
}

// remoteUDP 是经 mesh 到目标节点的通道。
type remoteUDP struct {
	ctl     net.Conn       // 握手用的 TCP 连接，之后做控制连接
	pc      net.PacketConn // 本端在 mesh 上的 UDP 端口
	peer    *net.UDPAddr   // 对方在 mesh 上的 UDP 地址
	stop    chan struct{}
	once    sync.Once
	poke    atomic.Int64 // 上次问对方的时间（UnixNano）
	unacked atomic.Int64 // 最早一个还没回应的问话的时间，0 表示都回了
}

func (r *remoteUDP) send(msg []byte) {
	_, _ = r.pc.WriteTo(msg, r.peer)
	now := time.Now().UnixNano()
	if last := r.poke.Load(); now-last >= int64(udpPoke) && r.poke.CompareAndSwap(last, now) {
		r.unacked.CompareAndSwap(0, now)
		go func() {
			_ = r.ctl.SetWriteDeadline(time.Now().Add(udpDeadAfter))
			_, _ = r.ctl.Write([]byte("?"))
		}()
	}
}

// watch 在问话 udpPokeTimeout 没有回应时拆掉通道。
func (r *remoteUDP) watch() {
	t := time.NewTicker(udpPokeTimeout / 6)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			if u := r.unacked.Load(); u != 0 && now.UnixNano()-u > int64(udpPokeTimeout) {
				r.close()
				return
			}
		}
	}
}
func (r *remoteUDP) done() <-chan struct{} { return r.stop }
func (r *remoteUDP) close() {
	r.once.Do(func() {
		close(r.stop)
		r.ctl.Close()
		r.pc.Close()
	})
}

// dialRemoteUDP 经 mesh 请目标节点开一条到它上面的容器的 UDP 通道。对方说"不在这里"或者联系不上时返回 nil。
func (n *intranet) dialRemoteUDP(k mesh.Key, name string, p *nagiv1.LinkProxy, port uint16, reply func([]byte)) udpTunnel {
	if n.a.mesh == nil {
		return nil
	}
	pc, err := n.a.mesh.m.ListenUDP(0)
	if err != nil {
		n.a.log.Debug("在 mesh 上开 UDP 端口失败", "err", err)
		return nil
	}
	local, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		pc.Close()
		return nil
	}
	ctx, cancel := context.WithTimeout(n.a.ctx, udpDial)
	mc, err := n.a.mesh.m.Dial(ctx, k, linkPort)
	cancel()
	if err != nil {
		n.a.log.Debug("经 mesh 连接目标节点失败", "name", p.GetAlias(), "node", name, "err", err)
		pc.Close()
		return nil
	}
	fail := func(msg string, args ...any) udpTunnel {
		n.a.log.Debug(msg, append([]any{"name", p.GetAlias(), "node", name}, args...)...)
		mc.Close()
		pc.Close()
		return nil
	}
	if _, err := fmt.Fprintf(mc, "udp %s %s %d %d\n", p.GetTargetProject(), p.GetTargetName(), port, local.Port); err != nil {
		return fail("发送 UDP 通道请求失败", "err", err)
	}
	mbr := bufio.NewReader(mc)
	_ = mc.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(mbr)
	if err != nil {
		return fail("目标节点没有回应 UDP 通道请求", "err", err)
	}
	rest, ok := strings.CutPrefix(line, "OK ")
	rport, perr := strconv.ParseUint(rest, 10, 16)
	if !ok || perr != nil || rport == 0 {
		return fail("目标节点没有接受 UDP 通道", "reply", line)
	}
	_ = mc.SetReadDeadline(time.Time{})
	r := &remoteUDP{ctl: mc, pc: pc, peer: &net.UDPAddr{IP: mesh.Address(k).AsSlice(), Port: int(rport)}, stop: make(chan struct{})}
	r.poke.Store(time.Now().UnixNano())
	go func() {
		// 对方发来的任何东西都说明它还在。
		keepAlive(mc, mbr, r.stop, nil, func(byte) { r.unacked.Store(0) })
		r.close()
	}()
	go r.watch()
	go func() {
		defer r.close()
		buf := make([]byte, udpBufSize)
		for {
			m, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// mesh 上的来源地址已经由 WireGuard 认证过，这里只收对方这条通道的端口发来的。
			if ua, ok := from.(*net.UDPAddr); !ok || ua.Port != r.peer.Port || !ua.IP.Equal(r.peer.IP) {
				continue
			}
			if m >= 4 && m <= 4+udpMaxDatagram {
				reply(buf[:m])
			}
		}
	}()
	return r
}

// ---- 目标这一侧：经 mesh 连进来 ----

// handleRemoteUDP 处理其他节点的 UDP 通道请求（"udp 项目 名字 端口 对方的 UDP 端口"），检查和 TCP 一样。
func (n *intranet) handleRemoteUDP(ctx context.Context, c net.Conn, br *bufio.Reader, caller mesh.Key, req string) {
	reply := func(msg string) { fmt.Fprintf(c, "%s\n", msg) }
	f := strings.Fields(req)
	if len(f) != 4 {
		reply("ERR 请求格式不对")
		return
	}
	projectID := f[0]
	name, port, err := parseLinkHeader(f[1] + " " + f[2])
	peerPort, perr := strconv.ParseUint(f[3], 10, 16)
	if err != nil || perr != nil || peerPort == 0 {
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
	// 只转发开放的 UDP 端口（容器声明的和项目设置里补的），理由同 TCP。
	if !slices.Contains(e.UDPPorts, uint32(port)) && !slices.Contains(spec.InternalUDPPortMap(internal)[name], uint32(port)) {
		reply(fmt.Sprintf("ERR %s 没有开放 UDP 端口 %d", name, port))
		return
	}
	// 连接到调用方的 UDP 端口：只收得到调用方（WireGuard 认证过的 mesh 地址）这个端口发来的包。
	uc, err := n.a.mesh.m.DialUDP(caller, uint16(peerPort))
	if err != nil {
		reply("ERR " + err.Error())
		return
	}
	defer uc.Close()
	local, ok := uc.LocalAddr().(*net.UDPAddr)
	if !ok {
		reply("ERR 在 mesh 上开 UDP 端口失败")
		return
	}
	flows := newUDPFlows(netip.AddrPortFrom(e.IP, port), func(msg []byte) { _, _ = uc.Write(msg) })
	defer flows.close()
	if _, err := fmt.Fprintf(c, "OK %d\n", local.Port); err != nil {
		return
	}
	go func() {
		defer flows.close()
		buf := make([]byte, udpBufSize)
		for {
			m, err := uc.Read(buf)
			if err != nil {
				return
			}
			if m >= 4 && m <= 4+udpMaxDatagram {
				flows.send(buf[:m])
			}
		}
	}()
	// 容器不在了或者换了地址（重建、被顶替走）时拆掉通道，调用方重新找。
	keepAlive(c, br, flows.done(), func() bool {
		cur, ok := n.endpoint(ctx, projectID, name)
		return ok && cur.IP == e.IP
	}, func(b byte) {
		if b == '?' {
			_ = c.SetWriteDeadline(time.Now().Add(udpDeadAfter))
			_, _ = c.Write([]byte("\n"))
		}
	})
}
