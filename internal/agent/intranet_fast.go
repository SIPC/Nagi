package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 项目内网跨节点连接的两处提速（见 docs/agent.md#项目内网）：
//   - 目标节点记住容器的地址，不用每个连进来的连接都查一次 Docker（ContainerList，实测 5～10 ms，容器多时更慢）；
//   - 本节点不等对方回 OK 就把客户端已经发来的数据跟在请求后面发出去，新连接少一个往返。对方拒绝（不在这里、
//     联系不上）时，这些数据和请求一起重发给下一个节点。对方按顺序先读请求行，后面的数据留在缓冲里，
//     连上容器后原样转过去，旧版本的节点也是这样，所以不用改协议。

// ---- 目标节点：容器地址的缓存 ----

// endpointTTL 是缓存的容器地址最多用多久。平时靠 Docker 事件清空（Nagi 管理的容器有变化、任何容器接入网络），
// 这只是兜底：事件流断开重连期间漏掉的变化。
const endpointTTL = 10 * time.Second

type cachedEndpoint struct {
	e   project.Endpoint
	at  time.Time
	gen uint64
}

type endpointCache struct {
	mu  sync.Mutex
	gen uint64 // 清空一次加一：清空之前开始的查询，结果不再记进缓存
	m   map[string]cachedEndpoint
}

// forgetEndpoints 清空缓存的容器地址。容器启动、停止、重建、接入网络时调用。
func (n *intranet) forgetEndpoints() {
	n.eps.mu.Lock()
	defer n.eps.mu.Unlock()
	n.eps.gen++
	clear(n.eps.m)
}

// lookupEndpoint 找本节点上项目里的容器，先看缓存。cached 表示用的是缓存：连不上时用 refreshEndpoint 重新查。
// 只缓存找到的：找不到时每次都查，刚启动的容器马上能连。
func (n *intranet) lookupEndpoint(ctx context.Context, projectID, name string) (e project.Endpoint, found, cached bool) {
	key := projectID + "/" + name
	n.eps.mu.Lock()
	if c, ok := n.eps.m[key]; ok && c.gen == n.eps.gen && time.Since(c.at) < endpointTTL {
		n.eps.mu.Unlock()
		return c.e, true, true
	}
	gen := n.eps.gen
	n.eps.mu.Unlock()
	e, found = n.endpoint(ctx, projectID, name)
	if found {
		n.eps.mu.Lock()
		if gen == n.eps.gen {
			if n.eps.m == nil {
				n.eps.m = map[string]cachedEndpoint{}
			}
			n.eps.m[key] = cachedEndpoint{e: e, at: time.Now(), gen: gen}
		}
		n.eps.mu.Unlock()
	}
	return e, found, false
}

// refreshEndpoint 去掉缓存里的这个容器，重新查。
func (n *intranet) refreshEndpoint(ctx context.Context, projectID, name string) (project.Endpoint, bool) {
	n.eps.mu.Lock()
	delete(n.eps.m, projectID+"/"+name)
	n.eps.mu.Unlock()
	e, found, _ := n.lookupEndpoint(ctx, projectID, name)
	return e, found
}

// dialEndpoint 连到容器的端口。用的是缓存的地址又连不上时（容器重建换了地址，事件还没到），重新查一次再连。
// 返回实际用的地址（端口检查要用它声明的端口）。
func (n *intranet) dialEndpoint(ctx context.Context, projectID, name string, port uint16) (net.Conn, project.Endpoint, bool, error) {
	e, found, cached := n.lookupEndpoint(ctx, projectID, name)
	if !found {
		return nil, e, false, nil
	}
	tc, err := net.DialTimeout("tcp", netip.AddrPortFrom(e.IP, port).String(), 5*time.Second)
	if err == nil || !cached {
		return tc, e, true, err
	}
	fresh, found := n.refreshEndpoint(ctx, projectID, name)
	if !found {
		return nil, fresh, false, nil
	}
	if fresh.IP == e.IP {
		return nil, fresh, true, err
	}
	tc, err = net.DialTimeout("tcp", netip.AddrPortFrom(fresh.IP, port).String(), 5*time.Second)
	return tc, fresh, true, err
}

// ---- 本节点：不等 OK 先发数据 ----

// earlyLimit 是对方回 OK 之前最多先发多少客户端数据（重试时要重发，所以要留着）。超过以后等 OK 再读。
const earlyLimit = 64 << 10

// clientPump 在后台读客户端的数据，一块一块交出去。读完（客户端关了写方向）时关掉 chunks。
type clientPump struct {
	chunks chan []byte
	done   chan struct{}
	err    error // 读客户端出错（不是 EOF）
}

func newClientPump(r io.Reader) *clientPump {
	p := &clientPump{chunks: make(chan []byte), done: make(chan struct{})}
	go func() {
		defer close(p.chunks)
		for {
			b := make([]byte, 32<<10)
			k, err := r.Read(b)
			if k > 0 {
				select {
				case p.chunks <- b[:k]:
				case <-p.done:
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					p.err = err
				}
				return
			}
		}
	}()
	return p
}

func (p *clientPump) stop() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// pending 是对方确认之前已经读出来的客户端数据：重试下一个节点时连同请求一起重发。
type pending struct {
	sent [][]byte
	size int
	eof  bool // 客户端已经关了写方向
}

func (p *pending) add(b []byte) {
	p.sent = append(p.sent, b)
	p.size += len(b)
}

func (p *pending) writeTo(w io.Writer) error {
	for _, b := range p.sent {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// forward 按计划里的顺序找目标所在的节点，把连接 c（数据从 r 读）转给目标的端口 port：先发数据，确认以后双向转发。
func (n *intranet) forward(c net.Conn, r io.Reader, p *nagiv1.LinkProxy, port uint16) {
	targets := p.GetNodes()
	if len(targets) > maxProxyTarget {
		targets = targets[:maxProxyTarget]
	}
	pump := newClientPump(r)
	defer pump.stop()
	var early pending
	for _, t := range targets {
		k, err := mesh.ParseKey(t.GetKey())
		if err != nil {
			continue
		}
		if n.a.mesh != nil && k == n.a.mesh.m.PublicKey() {
			// 目标就在本节点上（例如连接了同一个节点上的另一个项目）。
			tc, _, found, err := n.dialEndpoint(n.a.ctx, p.GetTargetProject(), p.GetTargetName(), port)
			if !found || err != nil {
				continue
			}
			if early.writeTo(tc) == nil {
				relay(c, tc, tc, pump, early.eof)
			}
			tc.Close()
			return
		}
		mc, mbr, ok := n.tryRemote(k, t.GetName(), p, port, pump, &early)
		if !ok {
			continue
		}
		relay(c, mc, mbr, pump, early.eof)
		mc.Close()
		return
	}
}

// tryRemote 经 mesh 请目标节点连到它上面的容器：发请求和已经读出的客户端数据，等 OK 的同时接着发客户端新来的数据
// （最多 earlyLimit）。对方回别的、连不上或者 10 秒没回时返回 false，读出的数据都记在 early 里，交给下一个节点。
func (n *intranet) tryRemote(k mesh.Key, name string, p *nagiv1.LinkProxy, port uint16, pump *clientPump, early *pending) (net.Conn, *bufio.Reader, bool) {
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
	// 请求和已有的数据合在一次写里，通常一个包就发出去了。
	head := fmt.Appendf(nil, "%s %s %d\n", p.GetTargetProject(), p.GetTargetName(), port)
	for _, b := range early.sent {
		head = append(head, b...)
	}
	if _, err := mc.Write(head); err != nil {
		mc.Close()
		return nil, nil, false
	}
	if early.eof {
		closeWrite(mc)
	}
	mbr := bufio.NewReader(mc)
	_ = mc.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply := make(chan string, 1)
	go func() {
		line, err := readLine(mbr)
		if err != nil {
			line = ""
		}
		reply <- line
	}()
	chunks := pump.chunks
	for {
		if early.eof || early.size >= earlyLimit {
			chunks = nil // 客户端发完了，或者先发的已经够多：等 OK
		}
		select {
		case b, ok := <-chunks:
			if !ok {
				early.eof = true
				closeWrite(mc)
				continue
			}
			early.add(b)
			_, _ = mc.Write(b) // 写失败时下面读 OK 也会失败
		case line := <-reply:
			if line != "OK" {
				n.a.log.Debug("目标节点没有接受连接", "name", p.GetAlias(), "node", name, "reply", line)
				mc.Close()
				return nil, nil, false
			}
			_ = mc.SetReadDeadline(time.Time{})
			early.sent, early.size = nil, 0
			return mc, mbr, true
		}
	}
}

// relay 在客户端 c 和目标之间双向转发：客户端的数据从 pump 来（eof 表示已经读完、写方向已经关了），
// 目标的数据从 tr 读。和 splice 一样，一边写完后关闭对方的写方向，两边都结束才返回。
func relay(c net.Conn, t net.Conn, tr io.Reader, pump *clientPump, eof bool) {
	done := make(chan struct{}, 2)
	go func() {
		if !eof {
			for b := range pump.chunks {
				if _, err := t.Write(b); err != nil {
					pump.stop()
					break
				}
			}
			closeWrite(t)
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, tr)
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}
