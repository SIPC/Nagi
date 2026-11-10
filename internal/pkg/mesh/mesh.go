package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// MTU 用 IPv6 的最小值，加上 WireGuard 和中转的包头也不会超过常见的 1500。
const MTU = 1280

// Peer 是一个 mesh 成员。
type Peer struct {
	Key Key
	// Endpoints 是可以尝试直连的 UDP 地址。
	Endpoints []netip.AddrPort
	// Relays 是可以代为中转的面板（面板的公钥）。
	Relays []Key
	// NodeRelays 是可以代为中转的节点，按延迟从低到高，先于面板使用（见 docs/mesh.md#经节点中转）。
	NodeRelays []Key
}

// Config 是 mesh 的配置。
type Config struct {
	PrivateKey Key
	// Port 是 UDP 端口，0 表示随机。
	Port uint16
	// Relay 为 true 时替别的成员中转（面板）。
	Relay bool
	// NodeRelay 为 true 时替别的节点中转：只在和收发两端都直连时经 UDP 转发，按来源限速（见 Bind.relayFor）。
	NodeRelay bool
	// ExtraPorts 是主端口以外再开的 UDP 端口数（随机端口，只用来往外发），直连不通时从这些端口也探测一遍，
	// 避开线路上负载均衡到坏链路的流（见 Bind.extra）。0 表示只用主端口。
	ExtraPorts int
	// NoFEC、NoQUIC 关掉 FEC（见 fec.go）、QUIC（见 quic.go），只用于测试对比。
	NoFEC  bool
	NoQUIC bool
	Logger *slog.Logger
}

// SetNodeRelay 开关替别的节点中转（节点的网络设置）。
func (m *Mesh) SetNodeRelay(on bool) { m.bind.nodeRelay.Store(on) }

// NodeRelay 返回是否替别的节点中转。
func (m *Mesh) NodeRelay() bool { return m.bind.nodeRelay.Load() }

// Direct 是和某个成员的直连。
type Direct struct {
	Key Key
	RTT time.Duration
}

// Directs 返回现在直连着的成员和延迟（上报给面板，面板据此挑中转节点）。
func (m *Mesh) Directs() []Direct {
	var out []Direct
	for _, s := range m.bind.status() {
		if s.Path == "direct" {
			out = append(out, Direct{Key: s.Key, RTT: s.RTT})
		}
	}
	return out
}

// DefaultExtraPorts 是节点和面板默认开的备用端口数。每多一个，所有端口都落在坏链路上的概率减半。
const DefaultExtraPorts = 4

// Mesh 是一个运行中的 mesh 成员。
type Mesh struct {
	bind *Bind
	dev  *device.Device
	tnet *netstack.Net
	quic *quicNet // 为 nil 时不用 QUIC
	self Key
	addr netip.Addr
	log  *slog.Logger

	mu         sync.Mutex
	configured map[Key]bool
	done       chan struct{}
	closeOnce  sync.Once
}

// New 启动 mesh。
func New(cfg Config) (*Mesh, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	self := PublicKey(cfg.PrivateKey)
	addr := Address(self)
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, MTU)
	if err != nil {
		return nil, err
	}
	var caps byte
	if !cfg.NoFEC {
		caps |= capFEC
	}
	if !cfg.NoQUIC {
		caps |= capQUIC
	}
	bind := newBind(cfg.PrivateKey, cfg.Relay, caps, cfg.ExtraPorts, log)
	bind.nodeRelay.Store(cfg.NodeRelay)
	dev := device.NewDevice(tun, bind, &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   func(f string, a ...any) { log.Debug("wireguard: " + fmt.Sprintf(f, a...)) },
	})
	if err := dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n", cfg.PrivateKey.hex(), cfg.Port)); err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("启动 WireGuard（UDP 端口 %d）: %w", cfg.Port, err)
	}
	m := &Mesh{bind: bind, dev: dev, tnet: tnet, self: self, addr: addr, log: log,
		configured: map[Key]bool{}, done: make(chan struct{})}
	if !cfg.NoQUIC {
		q, err := newQUICNet(m, cfg.PrivateKey)
		if err != nil {
			dev.Close()
			return nil, err
		}
		m.quic = q
		bind.mu.Lock()
		bind.quic = q.pc
		// 对方支持 QUIC 时提前建好连接，第一个 Dial 不用等。两边只由公钥小的一方去建，免得同时建两条。
		// 对方刚打开 QUIC 时，之前连不上的记录作废。
		bind.onCaps = func(k Key) {
			q.unfail(k)
			if bytes.Compare(self[:], k[:]) < 0 {
				q.conn(context.Background(), k)
			}
		}
		bind.mu.Unlock()
	}
	go m.loop()
	return m, nil
}

func (m *Mesh) loop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-t.C:
			m.bind.maintain(now)
		}
	}
}

// SetQUIC 打开或关闭 QUIC 快速通道（见 quic.go）。关闭时断开已有的 QUIC 连接，并告诉各成员不再支持，
// 之后双方的 TCP 连接都走 WireGuard。用 NoQUIC 启动的 mesh 打不开。
func (m *Mesh) SetQUIC(on bool) {
	if m.quic == nil {
		return
	}
	b := m.bind
	now := time.Now()
	var peers []Key
	b.mu.Lock()
	was := b.caps&capQUIC != 0
	if on {
		b.caps |= capQUIC
		for k, p := range b.peers {
			if p.has(capQUIC, now) {
				peers = append(peers, k)
			}
		}
	} else {
		b.caps &^= capQUIC
	}
	onCaps := b.onCaps
	b.mu.Unlock()
	if was == on {
		return
	}
	if !on {
		m.quic.closeAll()
	}
	b.announce()
	if !on {
		return
	}
	// 对方收到能力包才接受连接，等一会儿再建（公钥大的一方由对方来建）。
	go func() {
		select {
		case <-time.After(time.Second):
		case <-m.done:
			return
		}
		for _, k := range peers {
			go onCaps(k)
		}
	}()
}

// dialWireGuard 经 WireGuard 里的 TCP 连接，不走 QUIC（测速对比用）。
func (m *Mesh) dialWireGuard(ctx context.Context, k Key, port uint16) (net.Conn, error) {
	return m.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(Address(k), port))
}

// quicOff 返回直连的 k 和这一端之间是不是有一端关了 QUIC：here 是这一端，peer 是对方。
// 对方支持 FEC（和 QUIC 同一个版本加的）却没声明 QUIC，才算对方关了，否则可能只是旧版本。
func (m *Mesh) quicOff(k Key) (here, peer bool) {
	if m.quic == nil {
		return false, false
	}
	now := time.Now()
	b := m.bind
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[k]
	if p == nil || !p.directAlive(now) {
		return false, false
	}
	return b.caps&capQUIC == 0, p.has(capFEC, now) && !p.has(capQUIC, now)
}

// QUIC 返回 QUIC 快速通道是否打开。
func (m *Mesh) QUIC() bool {
	if m.quic == nil {
		return false
	}
	m.bind.mu.Lock()
	defer m.bind.mu.Unlock()
	return m.bind.caps&capQUIC != 0
}

// Close 停止 mesh。可以重复调用。
func (m *Mesh) Close() {
	m.closeOnce.Do(func() {
		close(m.done)
		if m.quic != nil {
			m.quic.close()
		}
		m.dev.Close()
	})
}

// PublicKey 返回自己的公钥。
func (m *Mesh) PublicKey() Key { return m.self }

// Addr 返回自己的 mesh 地址。
func (m *Mesh) Addr() netip.Addr { return m.addr }

// Port 返回实际使用的 UDP 端口。
func (m *Mesh) Port() uint16 {
	m.bind.mu.Lock()
	defer m.bind.mu.Unlock()
	return m.bind.port
}

// SetPeers 更新成员列表。只增减有变化的成员，已有成员的 WireGuard 会话不受影响。
func (m *Mesh) SetPeers(peers []Peer) error {
	// 公钥是低阶点的成员整个不加：WireGuard 和控制包、QUIC 的认证都要和它算共享密钥，算出来是全零。
	valid := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if ValidPublicKey(p.Key) {
			valid = append(valid, p)
		} else {
			m.log.Warn("成员的公钥无效，不加入", "peer", p.Key.Short())
		}
	}
	peers = valid
	m.bind.setPeers(peers)
	want := map[Key]bool{}
	for _, p := range peers {
		if p.Key != m.self {
			want[p.Key] = true
		}
	}
	var b strings.Builder
	m.mu.Lock()
	for k := range m.configured {
		if !want[k] {
			fmt.Fprintf(&b, "public_key=%s\nremove=true\n", k.hex())
		}
	}
	for k := range want {
		if !m.configured[k] {
			fmt.Fprintf(&b, "public_key=%s\nendpoint=%s\nallowed_ip=%s/128\npersistent_keepalive_interval=25\n",
				k.hex(), k.hex(), Address(k))
		}
	}
	m.configured = want
	m.mu.Unlock()
	if m.quic != nil {
		m.quic.forget(want)
	}
	if b.Len() == 0 {
		return nil
	}
	err := m.dev.IpcSet(b.String())
	// 新成员立刻探测直连，不等下一轮：WireGuard 加上成员时就发起握手，路径一确认就补发。
	m.bind.maintain(time.Now())
	return err
}

// AttachLink 登记经控制连接到某个对端的通道，返回取消登记的函数。HandleLink 处理从这条通道收到的包。
func (m *Mesh) AttachLink(k Key, l Link) (detach func()) { return m.bind.AttachLink(k, l) }
func (m *Mesh) HandleLink(src, dst Key, payload []byte)  { m.bind.HandleLink(src, dst, payload) }

// Listen 在自己的 mesh 地址上监听 TCP。经 QUIC 来的连接（见 quic.go）也从这里收。
func (m *Mesh) Listen(port uint16) (net.Listener, error) {
	ln, err := m.tnet.ListenTCPAddrPort(netip.AddrPortFrom(m.addr, port))
	if err != nil || m.quic == nil {
		return ln, err
	}
	q := m.quic
	var l *meshListener
	l = newMeshListener(ln, func() {
		q.mu.Lock()
		if q.listeners[port] == l {
			delete(q.listeners, port)
		}
		q.mu.Unlock()
	})
	q.mu.Lock()
	q.listeners[port] = l
	q.mu.Unlock()
	return l, nil
}

// Dial 经 mesh 连接某个成员的 TCP 端口：能走 QUIC 就走 QUIC，否则走 WireGuard 里的 TCP。
func (m *Mesh) Dial(ctx context.Context, k Key, port uint16) (net.Conn, error) {
	if m.quic != nil {
		if c, err := m.quic.open(ctx, k, port); err == nil {
			return c, nil
		}
	}
	return m.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(Address(k), port))
}

// ListenUDP 在自己的 mesh 地址上开一个 UDP 端口，port 为 0 时随机选。
func (m *Mesh) ListenUDP(port uint16) (net.PacketConn, error) {
	return m.tnet.ListenUDPAddrPort(netip.AddrPortFrom(m.addr, port))
}

// DialUDP 从自己 mesh 地址上的随机端口连接某个成员的 UDP 端口，只收得到对方这个端口发来的包。
func (m *Mesh) DialUDP(k Key, port uint16) (net.Conn, error) {
	return m.tnet.DialUDPAddrPort(netip.AddrPortFrom(m.addr, 0), netip.AddrPortFrom(Address(k), port))
}

// KeyOf 返回 mesh 地址对应的成员。WireGuard 已经校验过来源，所以这就是对方的身份。
func (m *Mesh) KeyOf(addr net.Addr) (Key, bool) {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return Key{}, false
	}
	m.bind.mu.Lock()
	defer m.bind.mu.Unlock()
	k, ok := m.bind.byMesh[ap.Addr()]
	return k, ok
}

// Endpoints 返回可以告诉别人的直连地址：本机网卡地址，加上别人看到的我们的公网地址。
func (m *Mesh) Endpoints() []netip.AddrPort {
	out := m.localEndpoints()
	for _, a := range m.bind.observedAddrs() {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// localEndpoints 返回本机网卡上的地址（加上 mesh 的端口）。
func (m *Mesh) localEndpoints() []netip.AddrPort {
	port := m.Port()
	var out []netip.AddrPort
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skipInterface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil || p.Addr().IsLinkLocalUnicast() || Prefix.Contains(p.Addr()) {
				continue
			}
			out = append(out, netip.AddrPortFrom(p.Addr().Unmap(), port))
		}
	}
	return out
}

// NAT 按别的成员看到的这一端的地址（直连的 pong 里带回来的）判断这一端在不在 NAT 后面：
// none 表示看到的就是本机网卡上的地址和端口；nat 表示在 NAT 后面；symmetric 表示同一个出口地址上
// 看到了不同的端口，NAT 按目的地换端口，两端都在 NAT 后面时打洞基本不会成功。还没有人看到过时为空。
func (m *Mesh) NAT() string {
	var local []netip.Addr
	for _, e := range m.localEndpoints() {
		local = append(local, e.Addr())
	}
	return natKind(local, m.Port(), m.bind.observedBySocket())
}

// natKind 按每个本地端口被别人看到的地址判断 NAT（键是本地端口的下标，0 是主端口）。
// 同一个本地端口在同一个出口地址上被看到不同的端口，才是 NAT 按目的地换端口；不同的本地端口本来就映射成不同的端口。
func natKind(local []netip.Addr, port uint16, observed map[int][]netip.AddrPort) string {
	if len(observed) == 0 {
		return ""
	}
	public := false
	for i, list := range observed {
		ports := map[netip.Addr]map[uint16]bool{}
		for _, a := range list {
			if i == 0 && slices.Contains(local, a.Addr()) && a.Port() == port && !a.Addr().IsPrivate() {
				public = true
			}
			if ports[a.Addr()] == nil {
				ports[a.Addr()] = map[uint16]bool{}
			}
			ports[a.Addr()][a.Port()] = true
		}
		for _, ps := range ports {
			if len(ps) > 1 {
				return "symmetric"
			}
		}
	}
	if public {
		return "none"
	}
	return "nat"
}

// MaxEndpoints 是每个成员最多尝试的直连地址数，多给的不用。
const MaxEndpoints = maxCandidates

// UsableEndpoint 判断别的成员上报的地址能不能拿来尝试直连。
// 本机、链路本地、组播、广播、mesh 自己的地址不能用；内网地址可以（同一内网的节点靠它直连）。
func UsableEndpoint(ap netip.AddrPort) bool {
	a := ap.Addr().Unmap()
	if !a.IsValid() || ap.Port() == 0 {
		return false
	}
	if a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || Prefix.Contains(a) {
		return false
	}
	return a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

func skipInterface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "wg", "nagi", "tailscale", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Status 是到一个成员的连接状态。RxBytes、TxBytes 包括经 WireGuard 和经 QUIC 的。
type Status struct {
	PeerStatus
	LastHandshake time.Time
	RxBytes       uint64
	TxBytes       uint64
}

// Status 返回所有成员的连接状态。
func (m *Mesh) Status() []Status {
	stats := map[Key]*Status{}
	var out []Status
	for _, s := range m.bind.status() {
		stats[s.Key] = &Status{PeerStatus: s}
	}
	cfg, err := m.dev.IpcGet()
	if err == nil {
		var cur *Status
		sc := bufio.NewScanner(strings.NewReader(cfg))
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), "=")
			switch k {
			case "public_key":
				cur = nil
				if raw, err := hex.DecodeString(v); err == nil {
					if key, err := ParseKey(raw); err == nil {
						cur = stats[key]
					}
				}
			case "last_handshake_time_sec":
				if sec, _ := strconv.ParseInt(v, 10, 64); cur != nil && sec > 0 {
					cur.LastHandshake = time.Unix(sec, 0)
				}
			case "rx_bytes":
				if cur != nil {
					cur.RxBytes, _ = strconv.ParseUint(v, 10, 64)
				}
			case "tx_bytes":
				if cur != nil {
					cur.TxBytes, _ = strconv.ParseUint(v, 10, 64)
				}
			}
		}
	}
	for _, s := range stats {
		s.QUIC = m.quic != nil && m.quic.has(s.Key)
		s.RxBytes = addBytes(s.RxBytes+s.quicBytes[0], s.fecBytes[0])
		s.TxBytes = addBytes(s.TxBytes+s.quicBytes[1], s.fecBytes[1])
		if m.quic != nil && s.Path == "direct" {
			if ppm, at := m.quic.txLoss(s.Key); time.Since(at) < lossFresh && at.After(s.txLossAt) {
				s.TxLoss = int64(ppm)
			}
		}
		out = append(out, *s)
	}
	return out
}

// addBytes 把 FEC 多出（可能为负，见 fecDecoder.overhead）的字节数加到计数上，不小于 0。
func addBytes(n uint64, d int64) uint64 {
	if d < 0 && uint64(-d) > n {
		return 0
	}
	return uint64(int64(n) + d)
}

// RelayBytes 返回替别人中转（面板）收到、转出的累计字节数，不含包头。
func (m *Mesh) RelayBytes() (rx, tx uint64) { return m.bind.relayRx.Load(), m.bind.relayTx.Load() }
