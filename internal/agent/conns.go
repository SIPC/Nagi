package agent

// 网络连接（见 docs/agent.md#网络连接）：面板打开节点的「网络连接」时，节点每隔几秒用 netlink sock_diag
// 列出本机和各容器网络命名空间里的连接，按 socket inode 找到持有它的进程，按 cgroup 找到容器。
// 只在有人看的时候采样，几个人同时看共用一份；结果只在内存里，不写盘。

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// connsInterval 是平时的采样间隔；一轮超过 connsSlowTook 就放慢到 connsSlowInterval，降到 connsFastTook 以下再恢复。
	connsInterval     = 2 * time.Second
	connsSlowInterval = 5 * time.Second
	connsSlowTook     = 300 * time.Millisecond
	connsFastTook     = 100 * time.Millisecond
	// connsMax 是一轮最多发的连接数（按速率排序），其余只计数。
	connsMax = 2000
	// connsMaxWatchers 是同时看的人数上限（所有面板合计）。
	connsMaxWatchers = 4
	// connsMaxWatch 是一次最长看多久，到时结束，界面提示继续。
	connsMaxWatch = 30 * time.Minute
	// cmdlineMax 是命令行最多保留的字节数。
	cmdlineMax = 512
)

// sampledConn 是采样到的一条连接、区分它的 key（网络命名空间、两端、inode）和 socket inode。这两项不发给面板。
type sampledConn struct {
	key   string
	inode uint32
	c     *nagiv1.NodeConnection
}

// connSample 是一轮采样的结果。
type connSample struct {
	conns   []sampledConn
	took    time.Duration
	nsError int
}

// connWatch 管理采样：有人订阅时在后台按间隔采样，最后一个人退订后停下。
type connWatch struct {
	// sample 做一轮采样。cmdline 为 false 时不读命令行。
	sample func(cmdline bool) (connSample, error)

	mu   sync.Mutex
	subs map[*connSub]bool
	stop chan struct{}
}

type connSub struct {
	cmdline bool
	// ch 里总是最新的一份：来不及取的旧结果直接换掉。
	ch chan *nagiv1.ConnectionSnapshot
}

func newConnWatch(sample func(cmdline bool) (connSample, error)) *connWatch {
	return &connWatch{sample: sample, subs: map[*connSub]bool{}}
}

var errConnsBusy = status.Error(codes.ResourceExhausted, "同时查看网络连接的人数已达上限，请稍后再试")

// subscribe 订阅采样结果，返回退订的函数。
func (w *connWatch) subscribe(cmdline bool) (*connSub, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subs) >= connsMaxWatchers {
		return nil, nil, errConnsBusy
	}
	s := &connSub{cmdline: cmdline, ch: make(chan *nagiv1.ConnectionSnapshot, 1)}
	w.subs[s] = true
	if w.stop == nil {
		w.stop = make(chan struct{})
		go w.loop(w.stop)
	}
	return s, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.subs, s)
		if len(w.subs) == 0 && w.stop != nil {
			close(w.stop)
			w.stop = nil
		}
	}, nil
}

func (w *connWatch) loop(stop chan struct{}) {
	var prev map[string]connCounters
	var prevAt time.Time
	interval := connsInterval
	for {
		w.mu.Lock()
		cmdline := false
		for s := range w.subs {
			cmdline = cmdline || s.cmdline
		}
		w.mu.Unlock()
		sample, err := w.sample(cmdline)
		var snap *nagiv1.ConnectionSnapshot
		if err == nil {
			now := time.Now()
			prev = applyRates(sample.conns, prev, now.Sub(prevAt))
			prevAt = now
			interval = nextConnsInterval(interval, sample.took)
			snap = buildSnapshot(sample, interval)
		}
		// 采样失败（读不了 /proc 之类）这一轮不发，下一轮再试；第一轮就失败的话界面一直是加载中，由 StreamConnections 报错。
		w.mu.Lock()
		for s := range w.subs {
			if snap == nil {
				continue
			}
			out := snap
			if !s.cmdline && cmdline {
				out = withoutCmdline(snap)
			}
			select {
			case <-s.ch:
			default:
			}
			s.ch <- out
		}
		w.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
	}
}

func nextConnsInterval(cur, took time.Duration) time.Duration {
	switch {
	case took > connsSlowTook:
		return connsSlowInterval
	case took < connsFastTook:
		return connsInterval
	}
	return cur
}

// connCounters 是一条连接上一轮的累计字节。
type connCounters struct{ sent, received uint64 }

// applyRates 按上一轮的累计字节算出速率，返回这一轮的累计字节。第一次看到的连接、计数变小的（不是同一条）没有速率。
func applyRates(conns []sampledConn, prev map[string]connCounters, elapsed time.Duration) map[string]connCounters {
	next := make(map[string]connCounters, len(conns))
	for _, sc := range conns {
		c := sc.c
		cur := connCounters{c.GetBytesSent(), c.GetBytesReceived()}
		next[sc.key] = cur
		p, ok := prev[sc.key]
		if !ok || elapsed <= 0 || cur.sent < p.sent || cur.received < p.received {
			continue
		}
		s := elapsed.Seconds()
		c.SendRate, c.ReceiveRate = float64(cur.sent-p.sent)/s, float64(cur.received-p.received)/s
	}
	return next
}

// buildSnapshot 按收发速率（一样时按累计字节）排序，截到 connsMax 条。
func buildSnapshot(s connSample, interval time.Duration) *nagiv1.ConnectionSnapshot {
	list := make([]*nagiv1.NodeConnection, len(s.conns))
	for i, sc := range s.conns {
		list[i] = sc.c
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := list[i].GetSendRate()+list[i].GetReceiveRate(), list[j].GetSendRate()+list[j].GetReceiveRate()
		if ri != rj {
			return ri > rj
		}
		return list[i].GetBytesSent()+list[i].GetBytesReceived() > list[j].GetBytesSent()+list[j].GetBytesReceived()
	})
	out := &nagiv1.ConnectionSnapshot{IntervalMs: uint32(interval.Milliseconds()), TookMs: uint32(s.took.Milliseconds()), NamespaceErrors: uint32(s.nsError)}
	if len(list) > connsMax {
		out.Omitted = uint32(len(list) - connsMax)
		list = list[:connsMax]
	}
	out.Connections = list
	return out
}

// withoutCmdline 复制一份去掉命令行的结果：同时看的人里有人要命令行时，不要的人拿这份。
func withoutCmdline(s *nagiv1.ConnectionSnapshot) *nagiv1.ConnectionSnapshot {
	out := proto.Clone(s).(*nagiv1.ConnectionSnapshot)
	for _, c := range out.GetConnections() {
		c.Cmdline = ""
	}
	return out
}

// streamConnections 把订阅到的结果发给面板，直到面板断开或到了 connsMaxWatch。
func streamConnections(ctx context.Context, w *connWatch, cmdline bool, send func(*nagiv1.ConnectionSnapshot) error) error {
	if w == nil {
		return status.Error(codes.Unimplemented, "这个节点不支持网络连接视图（只支持 Linux）")
	}
	sub, done, err := w.subscribe(cmdline)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, connsMaxWatch)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return ctx.Err()
		case snap := <-sub.ch:
			if err := send(snap); err != nil {
				return err
			}
		}
	}
}

// StreamConnections 是面板的「网络连接」视图（见 docs/agent.md#网络连接）。面板断开时停止。
func (s *agentService) StreamConnections(req *nagiv1.StreamConnectionsRequest, stream nagiv1.AgentService_StreamConnectionsServer) error {
	return streamConnections(stream.Context(), s.node.conns, req.GetCmdline(), stream.Send)
}

// ---- netlink sock_diag 的消息（linux/inet_diag.h）：头部是本机字节序，端口和地址是网络序 ----

const (
	nlmsgHdrLen     = 16
	nlmsgError      = 2
	nlmsgDone       = 3
	sockDiagByFam   = 20
	inetDiagReqLen  = 56
	inetDiagMsgLen  = 72
	inetDiagInfo    = 2
	nlmFRequest     = 0x1
	nlmFDump        = 0x300
	ipprotoTCP      = 6
	ipprotoUDP      = 17
	afInet          = 2
	afInet6         = 10
	tcpListen       = 10
	tcpInfoRTT      = 68
	tcpInfoAcked    = 120
	tcpInfoReceived = 128
	tcpInfoSent     = 200
)

var tcpStates = [...]string{1: "ESTABLISHED", 2: "SYN_SENT", 3: "SYN_RECV", 4: "FIN_WAIT1", 5: "FIN_WAIT2", 6: "TIME_WAIT",
	7: "CLOSE", 8: "CLOSE_WAIT", 9: "LAST_ACK", 10: "LISTEN", 11: "CLOSING", 12: "NEW_SYN_RECV"}

// diagRequest 是一次 dump 请求：一个地址族、协议的全部状态，TCP 带上 tcp_info。
func diagRequest(family, protocol uint8, seq uint32) []byte {
	b := make([]byte, nlmsgHdrLen+inetDiagReqLen)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], uint32(len(b)))
	ne.PutUint16(b[4:], sockDiagByFam)
	ne.PutUint16(b[6:], nlmFRequest|nlmFDump)
	ne.PutUint32(b[8:], seq)
	r := b[nlmsgHdrLen:]
	r[0], r[1] = family, protocol
	if protocol == ipprotoTCP {
		r[2] = 1 << (inetDiagInfo - 1)
	}
	ne.PutUint32(r[4:], 0xffffffff) // 全部状态
	return b
}

// diagSocket 是 dump 回来的一个 socket。
type diagSocket struct {
	protocol      uint8
	state         uint8
	local, remote netip.AddrPort
	inode         uint32
	// sent、received 是累计字节，rtt 是平滑往返时间（微秒），都来自 tcp_info。
	sent, received uint64
	rtt            uint32
}

var errDiagTruncated = errors.New("sock_diag 回复不完整")

// parseDiag 解析一段 dump 回复，done 表示已经收到结束标记。
func parseDiag(b []byte, protocol uint8) (out []diagSocket, done bool, err error) {
	ne := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		n := int(ne.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return out, false, errDiagTruncated
		}
		typ, body := ne.Uint16(b[4:]), b[nlmsgHdrLen:n]
		b = b[min(len(b), (n+3)&^3):]
		switch typ {
		case nlmsgDone:
			return out, true, nil
		case nlmsgError:
			if len(body) >= 4 {
				if code := int32(ne.Uint32(body)); code != 0 {
					return out, true, status.Errorf(codes.Internal, "sock_diag 返回错误 %d", -code)
				}
			}
			return out, true, nil
		case sockDiagByFam:
		default:
			continue
		}
		if len(body) < inetDiagMsgLen {
			return out, false, errDiagTruncated
		}
		s := diagSocket{protocol: protocol, state: body[1], inode: ne.Uint32(body[68:])}
		id := body[4:52]
		sport, dport := binary.BigEndian.Uint16(id[0:]), binary.BigEndian.Uint16(id[2:])
		// inet_diag_sockid：源端口、目的端口、源地址（16 字节，IPv4 只用前 4 个）、目的地址、接口、cookie。
		if body[0] == afInet {
			s.local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(id[4:8])), sport)
			s.remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(id[20:24])), dport)
		} else {
			s.local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(id[4:20])).Unmap(), sport)
			s.remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(id[20:36])).Unmap(), dport)
		}
		for attrs := body[inetDiagMsgLen:]; len(attrs) >= 4; {
			l := int(ne.Uint16(attrs[0:]))
			if l < 4 || l > len(attrs) {
				break
			}
			if ne.Uint16(attrs[2:]) == inetDiagInfo {
				parseTCPInfo(attrs[4:l], &s)
			}
			attrs = attrs[min(len(attrs), (l+3)&^3):]
		}
		out = append(out, s)
	}
	return out, false, nil
}

// parseTCPInfo 读 tcp_info 里的往返时间和累计字节。结构随内核版本变长：接收的字节 4.1 起有，发送的 4.19 起有，
// 之前只有已确认的发送字节（bytes_acked），拿它代替。
func parseTCPInfo(b []byte, s *diagSocket) {
	ne := binary.NativeEndian
	if len(b) >= tcpInfoRTT+4 {
		s.rtt = ne.Uint32(b[tcpInfoRTT:])
	}
	if len(b) >= tcpInfoReceived+8 {
		s.sent, s.received = ne.Uint64(b[tcpInfoAcked:]), ne.Uint64(b[tcpInfoReceived:])
	}
	if len(b) >= tcpInfoSent+8 {
		s.sent = ne.Uint64(b[tcpInfoSent:])
	}
}

// connsOf 把一个网络命名空间里 dump 到的 socket 换成连接。去掉监听的 TCP 和没连对端的 UDP；
// 本地端口是这个命名空间里在监听的端口（TCP LISTEN、没连对端的 UDP）时算入站。ns 用来区分不同命名空间里一样的地址。
func connsOf(ns string, sockets []diagSocket) []sampledConn {
	type port struct {
		proto uint8
		port  uint16
	}
	listening := map[port]bool{}
	for _, s := range sockets {
		if s.protocol == ipprotoTCP && s.state == tcpListen || s.protocol == ipprotoUDP && s.remote.Port() == 0 {
			listening[port{s.protocol, s.local.Port()}] = true
		}
	}
	var out []sampledConn
	for _, s := range sockets {
		if s.protocol == ipprotoTCP && s.state == tcpListen || s.remote.Port() == 0 {
			continue
		}
		c := &nagiv1.NodeConnection{Protocol: "tcp", LocalIp: s.local.Addr().String(), LocalPort: uint32(s.local.Port()),
			RemoteIp: s.remote.Addr().String(), RemotePort: uint32(s.remote.Port()), Inbound: listening[port{s.protocol, s.local.Port()}],
			BytesSent: s.sent, BytesReceived: s.received, RttUs: s.rtt}
		if s.protocol == ipprotoUDP {
			c.Protocol = "udp"
		} else if int(s.state) < len(tcpStates) {
			c.State = tcpStates[s.state]
		}
		key := ns + "|" + c.Protocol + "|" + s.local.String() + "|" + s.remote.String() + "|" + strconv.FormatUint(uint64(s.inode), 10)
		out = append(out, sampledConn{key: key, inode: s.inode, c: c})
	}
	return out
}

// ---- 进程所属的容器和 systemd 单元 ----

var dockerIDPattern = regexp.MustCompile(`(?:^|[/-])([0-9a-f]{64})(?:\.scope)?(?:/|$)`)

// cgroupOwner 从 /proc/<pid>/cgroup 的内容里找出 Docker 容器 ID；不在容器里时找 systemd 单元。
// cgroup v2 只有一行 0::/system.slice/docker-<id>.scope，v1 有好几行（.../docker/<id>）。
func cgroupOwner(content string) (containerID, unit string) {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if m := dockerIDPattern.FindAllStringSubmatch(path, -1); len(m) > 0 {
			return m[len(m)-1][1], ""
		}
		if unit == "" {
			segs := strings.Split(path, "/")
			for i := len(segs) - 1; i >= 0; i-- {
				if strings.HasSuffix(segs[i], ".service") {
					unit = segs[i]
					break
				}
			}
		}
	}
	return "", unit
}

// cleanCmdline 把 /proc/<pid>/cmdline 的 NUL 分隔换成空格，最多 cmdlineMax 字节（不截断半个字符）。
func cleanCmdline(raw []byte) string {
	s := strings.TrimRight(strings.ReplaceAll(string(raw), "\x00", " "), " ")
	if len(s) <= cmdlineMax {
		return s
	}
	cut := cmdlineMax
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut]
}
