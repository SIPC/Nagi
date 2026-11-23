package proxy

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/mholt/caddy-l4/layer4"
)

// TCP、UDP 映射的统计（见 docs/monitoring.md#tcp--udp-统计）：layer4 没有访问日志，每条 tcp/udp 规则在 proxy 前面加一个
// nagi_stats 处理器，把连接包一层，按规则累计新连接数、连不上容器的次数、收发字节和当前连接数。
// 字节在传输时就计入，开了很久的连接每个采样间隔都有流量。UDP 没有连接，caddy-l4 按客户端的地址和端口归成会话，
// 空闲超过 idle_timeout（默认 30 秒）结束，这里一个会话算一个连接。

func init() {
	caddy.RegisterModule(l4Stats{})
}

// l4Counter 是一条规则的计数。conns、fails、in、out 每次取统计时清零；active 是当前连接数，不清零。
type l4Counter struct {
	conns, fails, in, out atomic.Int64
	active                atomic.Int64
}

// l4Stats 是 layer4 的处理器 nagi_stats，Route 是 "<项目 ID>/<规则名>"。tcp+udp 的映射拆成的两条同名规则记在一起。
type l4Stats struct {
	Route string `json:"route,omitempty"`
}

// ConnectionRecord describes this ingress observer's lifetime, not the remote
// application's success. UDP is one caddy-l4 session, never one packet/request.
type ConnectionRecord struct {
	ID        string    `json:"connection_id"`
	Route     string    `json:"route"`
	Time      time.Time `json:"time"`
	Opened    time.Time `json:"opened"`
	Phase     string    `json:"phase"`
	Protocol  string    `json:"protocol"`
	Local     string    `json:"local"`
	Remote    string    `json:"remote"`
	Duration  float64   `json:"duration_seconds"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	EndReason string    `json:"end_reason,omitempty"`
	Error     string    `json:"error,omitempty"`
}

var connectionSequence atomic.Uint64

func OnConnection(fn func(ConnectionRecord)) {
	observer.mu.Lock()
	observer.onConnection = fn
	observer.mu.Unlock()
}

func (l4Stats) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "layer4.handlers.nagi_stats",
		New: func() caddy.Module { return new(l4Stats) },
	}
}

// Provision 记下加载的配置里有这条规则：节点重启后没有连接的映射也每次报 0，
// 告警基线看到的是「没有连接」而不是「数据中断」。重载时新配置先加载、旧配置后清理，所以按次数计。
func (h *l4Stats) Provision(caddy.Context) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.l4Loaded[h.Route]++
	return nil
}

// Cleanup 在配置卸载时调用；规则不在任何已加载的配置里以后不再报 0。
func (h *l4Stats) Cleanup() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.l4Loaded[h.Route]--; observer.l4Loaded[h.Route] <= 0 {
		delete(observer.l4Loaded, h.Route)
	}
	return nil
}

// Handle 计入一个新连接，把连接包一层交给后面的 proxy。proxy 返回错误说明连不上容器（连上以后传输中断不返回错误）。
func (h *l4Stats) Handle(cx *layer4.Connection, next layer4.Handler) error {
	c := observer.l4Open(h.Route)
	defer c.active.Add(-1)
	observer.mu.Lock()
	fn := observer.onConnection
	observer.mu.Unlock()
	cc := &countingConn{Conn: cx.Conn, c: c}
	e := ConnectionRecord{}
	if fn != nil {
		at := time.Now()
		e = ConnectionRecord{ID: fmt.Sprintf("%d-%d", at.UnixNano(), connectionSequence.Add(1)),
			Route: h.Route, Time: at, Opened: at, Phase: "open", Protocol: cx.RemoteAddr().Network(),
			Local: cx.LocalAddr().String(), Remote: cx.RemoteAddr().String()}
		fn(e)
	}
	err := next.Handle(cx.Wrap(cc))
	if err != nil {
		c.fails.Add(1)
	}
	if fn != nil {
		e.Time, e.Phase = time.Now(), "close"
		e.Duration = e.Time.Sub(e.Opened).Seconds()
		e.BytesIn, e.BytesOut = cc.in.Load(), cc.out.Load()
		e.EndReason = "observer_finished"
		if err != nil {
			e.EndReason, e.Error = "handler_error", err.Error()
		}
		fn(e)
	}
	return err
}

// l4Open 取规则的计数（没有就新建），计入一个新连接。在锁里加 active，TakeStats 不会删掉正在用的计数。
func (o *observerT) l4Open(route string) *l4Counter {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.l4[route]
	if c == nil {
		c = &l4Counter{}
		o.l4[route] = c
	}
	c.conns.Add(1)
	c.active.Add(1)
	return c
}

// takeL4 把 tcp/udp 规则的计数合进 out 并清零。没有连接、也没有新计数的规则删掉（规则可能已经删除）。调用方持有 o.mu。
// 先取 active：为 0 时没有连接在用这份计数（新连接要先拿锁），之后清零的值不会再变，删掉不丢计数。
// 已加载的配置里的规则即使没有计数也返回一条全 0 的统计。
func (o *observerT) takeL4(out map[string]RouteStats) {
	for k := range o.l4Loaded {
		s := out[k]
		s.L4, s.Loaded = true, true
		out[k] = s
	}
	for k, c := range o.l4 {
		s := out[k]
		s.L4 = true
		s.Active = int(c.active.Load())
		s.Conns += int(c.conns.Swap(0))
		s.Fails += int(c.fails.Swap(0))
		s.BytesIn += c.in.Swap(0)
		s.BytesOut += c.out.Swap(0)
		out[k] = s
		if s.Active == 0 && s.Conns == 0 && s.Fails == 0 && s.BytesIn == 0 && s.BytesOut == 0 {
			delete(o.l4, k)
		}
	}
}

// countingConn 统计客户端一侧的收发字节：读到的是入站，写出的是出站。
type countingConn struct {
	net.Conn
	c       *l4Counter
	in, out atomic.Int64
}

func (cc *countingConn) Read(p []byte) (int, error) {
	n, err := cc.Conn.Read(p)
	cc.c.in.Add(int64(n))
	cc.in.Add(int64(n))
	return n, err
}

func (cc *countingConn) Write(p []byte) (int, error) {
	n, err := cc.Conn.Write(p)
	cc.c.out.Add(int64(n))
	cc.out.Add(int64(n))
	return n, err
}

// CloseWrite 转给底层连接：proxy 在容器那边关闭写入时，靠它把客户端一侧也半关闭（TCP）。UDP 的连接没有，什么也不做，
// 和不包这一层时一样。
func (cc *countingConn) CloseWrite() error {
	if w, ok := cc.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

var _ layer4.NextHandler = (*l4Stats)(nil)
