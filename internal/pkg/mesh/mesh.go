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
}

// Config 是 mesh 的配置。
type Config struct {
	PrivateKey Key
	// Port 是 UDP 端口，0 表示随机。
	Port uint16
	// Relay 为 true 时替别的成员中转（面板）。
	Relay bool
	// NoFEC、NoQUIC 关掉 FEC（见 fec.go）、QUIC（见 quic.go），只用于测试对比。
	NoFEC  bool
	NoQUIC bool
	Logger *slog.Logger
}

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
	bind := newBind(cfg.PrivateKey, cfg.Relay, caps, log)
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
		bind.onCaps = func(k Key) {
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
	for _, a := range m.bind.observedAddrs() {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func skipInterface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "wg", "nagi", "tailscale", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Status 是到一个成员的连接状态。
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
		out = append(out, *s)
	}
	return out
}
