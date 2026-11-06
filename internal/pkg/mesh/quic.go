package mesh

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh/bbr"
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// QUIC 快速通道（见 docs/mesh.md#quic-快速通道）：两端都支持、并且直连时，mesh 上的 TCP 连接改走 QUIC，
// 拥塞控制用 BBR。mesh 里的 TCP（gVisor）在有丢包的长距离线路上很慢，BBR 不把丢包当拥堵，快得多。
//
//   - QUIC 包不经过 WireGuard，和它共用 UDP 端口，按第一个字节区分（QUIC 的第二高位总是 1）。只收已确认直连地址发来的。
//   - 身份：TLS 证书是临时生成的，不用来认人。握手后双方在第一个流上用 WireGuard 密钥协商出的共享密钥（和 ping 的 MAC
//     同一个）对 TLS 导出的会话值签名，证明自己是谁；会话值和这次 TLS 连接绑定，中间人转发也通不过。
//   - 每对成员之间一条 QUIC 连接，谁先连上就用谁的，两个方向都可以开流。每个 mesh TCP 连接是一个流，开头两个字节是目标端口。
//   - Dial 先试 QUIC，不行（对方不支持、不是直连、连不上）就照旧走 gVisor TCP；Listen 两种都收。上层不用改。
const (
	quicALPN = "nagi-mesh"
	// quicDialTimeout：现建 QUIC 连接最多等这么久，再久就先走 gVisor TCP。
	quicDialTimeout = 2 * time.Second
	// quicRetry：建不起来以后这么久内不再试。
	quicRetry = time.Minute
	proofLen  = 32
)

var quicMagic = []byte("NQ1")

// isQUIC：QUIC 包的第一个字节第二高位是 1（RFC 9000 的 fixed bit）。WireGuard 包（1～4）和 FEC 包（0x10、0x11）都是 0；
// 控制包以 "NAGI" 开头，'N' 的这一位也是 1，所以先按控制包解析，解析不了的才当 QUIC。
func isQUIC(pkt []byte) bool { return len(pkt) > 0 && pkt[0]&0x40 != 0 }

type quicPacket struct {
	data []byte
	from netip.AddrPort
}

// quicPacketConn 是给 quic-go 用的 UDP 连接：收的包由 Bind 转进来，发的包从 Bind 的 UDP 端口发出去。
type quicPacketConn struct {
	b    *Bind
	in   chan quicPacket
	done chan struct{}
	once sync.Once
}

func (c *quicPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt := <-c.in:
		return copy(p, pkt.data), net.UDPAddrFromAddrPort(pkt.from), nil
	case <-c.done:
		return 0, nil, net.ErrClosed
	}
}

func (c *quicPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errors.New("不是 UDP 地址")
	}
	ap := ua.AddrPort()
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	c.b.mu.Lock()
	conn := c.b.conn
	if k, ok := c.b.byAddr[ap]; ok {
		if peer := c.b.peers[k]; peer != nil {
			peer.quicTx += uint64(len(p))
		}
	}
	c.b.mu.Unlock()
	c.b.write(conn, p, ap)
	return len(p), nil
}

func (c *quicPacketConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *quicPacketConn) LocalAddr() net.Addr {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if c.b.conn == nil {
		return &net.UDPAddr{}
	}
	return c.b.conn.LocalAddr()
}

func (c *quicPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *quicPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *quicPacketConn) SetWriteDeadline(time.Time) error { return nil }

// deliver 把收到的 QUIC 包交给 quic-go，队列满了就丢（QUIC 会重传）。
func (c *quicPacketConn) deliver(pkt []byte, from netip.AddrPort) {
	select {
	case c.in <- quicPacket{append([]byte(nil), pkt...), from}:
	case <-c.done:
	default:
	}
}

// quicNet 管理到各成员的 QUIC 连接。
type quicNet struct {
	m   *Mesh
	pc  *quicPacketConn
	tr  *quic.Transport
	ln  *quic.Listener
	tls *tls.Config

	mu        sync.Mutex
	conns     map[Key]*quic.Conn
	all       map[*quic.Conn]Key // 包括同时建起来、没放进 conns 的那一条
	dialing   map[Key]chan struct{}
	failed    map[Key]time.Time
	listeners map[uint16]*meshListener
	closed    bool
	loss      map[Key]*quicLoss
	counters  map[*quic.Conn]*countingCC
}

// quicLoss 按窗口算到某个对端的 QUIC 丢包率：发够 quicLossSamples 个包算一次。
type quicLoss struct {
	conn       *quic.Conn
	sent, lost uint64
	ppm        uint32
	at         time.Time // ppm 算出的时间，零值表示还没算过
}

const quicLossSamples = 1000

func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           5 * time.Second,
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
		MaxIncomingStreams:             4096,
	}
}

func newQUICNet(m *Mesh, priv Key) (*quicNet, error) {
	cert, err := ephemeralCert()
	if err != nil {
		return nil, err
	}
	q := &quicNet{
		m:  m,
		pc: &quicPacketConn{b: m.bind, in: make(chan quicPacket, 4096), done: make(chan struct{})},
		tls: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{quicALPN},
			InsecureSkipVerify: true, // 不靠证书认人，见上面的说明
			MinVersion:         tls.VersionTLS13,
		},
		conns: map[Key]*quic.Conn{}, all: map[*quic.Conn]Key{}, dialing: map[Key]chan struct{}{}, failed: map[Key]time.Time{},
		listeners: map[uint16]*meshListener{}, loss: map[Key]*quicLoss{}, counters: map[*quic.Conn]*countingCC{},
	}
	// 无状态重置的密钥由私钥推出：重启以后还是同一个，对方发来旧连接的包时能直接告诉它连接没了，
	// 不用等 MaxIdleTimeout。
	reset := quic.StatelessResetKey(sha256.Sum256(append([]byte("nagi mesh quic reset"), priv[:]...)))
	q.tr = &quic.Transport{Conn: q.pc, StatelessResetKey: &reset}
	q.ln, err = q.tr.Listen(q.tls, quicConfig())
	if err != nil {
		return nil, err
	}
	go q.accept()
	return q, nil
}

func (q *quicNet) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.closeAll()
	_ = q.ln.Close()
	// 先关包连接：Transport.Close 要等它的收包循环退出，收包循环停在 ReadFrom 上。
	_ = q.pc.Close()
	_ = q.tr.Close()
}

func ephemeralCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(100 * 365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// useBBR 把一条 QUIC 连接的拥塞控制换成 BBR，并在外面包一层数发出和丢掉的包（quic-go 换了拥塞控制以后
// ConnectionStats 里不再统计丢包）。
func (q *quicNet) useBBR(c *quic.Conn) {
	size := bbr.GetInitialPacketSize(c.RemoteAddr())
	if s := c.InitialPacketSize(); s > 0 && s < size {
		size = s
	}
	cc := &countingCC{CongestionControlEx: bbr.NewBbrSender(bbr.DefaultClock{}, size, bbr.Profile(""))}
	c.SetCongestionControl(cc)
	q.mu.Lock()
	q.counters[c] = cc
	q.mu.Unlock()
	go func() {
		<-c.Context().Done()
		q.mu.Lock()
		delete(q.counters, c)
		q.mu.Unlock()
	}()
}

// countingCC 数一条连接发出、丢掉的包（用来算丢包率），其余交给 BBR。
type countingCC struct {
	congestion.CongestionControlEx
	sent, lost atomic.Uint64
}

func (c *countingCC) OnPacketSent(t monotime.Time, inFlight congestion.ByteCount, pn congestion.PacketNumber, n congestion.ByteCount, retransmittable bool) {
	c.sent.Add(1)
	c.CongestionControlEx.OnPacketSent(t, inFlight, pn, n, retransmittable)
}

func (c *countingCC) OnCongestionEvent(pn congestion.PacketNumber, lost, priorInFlight congestion.ByteCount) {
	c.lost.Add(1)
	c.CongestionControlEx.OnCongestionEvent(pn, lost, priorInFlight)
}

// proof 是一端对这条 QUIC 连接的身份证明：HMAC(和对方的共享密钥, 角色 || TLS 导出的会话值)。
func proof(mac [32]byte, role string, c *quic.Conn) ([]byte, error) {
	state := c.ConnectionState().TLS
	ekm, err := state.ExportKeyingMaterial("nagi mesh quic", nil, 32)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, mac[:])
	h.Write([]byte(role))
	h.Write(ekm)
	return h.Sum(nil)[:proofLen], nil
}

// peerFor 返回可以走 QUIC 的对端：支持 QUIC、直连可用。返回直连地址和共享密钥。
func (q *quicNet) peerFor(k Key) (netip.AddrPort, [32]byte, bool) {
	b := q.m.bind
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[k]
	if p == nil || b.caps&capQUIC == 0 || !p.has(capQUIC, now) || !p.directAlive(now) {
		return netip.AddrPort{}, [32]byte{}, false
	}
	return p.direct, p.mac, true
}

// conn 返回到 k 的 QUIC 连接，没有时现建（最多等 quicDialTimeout）。不能走 QUIC 时返回 nil。
func (q *quicNet) conn(ctx context.Context, k Key) *quic.Conn {
	addr, mac, ok := q.peerFor(k)
	if !ok {
		return nil
	}
	for {
		q.mu.Lock()
		if c := q.conns[k]; c != nil {
			q.mu.Unlock()
			return c
		}
		if q.closed || time.Since(q.failed[k]) < quicRetry {
			q.mu.Unlock()
			return nil
		}
		if wait, ok := q.dialing[k]; ok {
			q.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		done := make(chan struct{})
		q.dialing[k] = done
		q.mu.Unlock()

		c, err := q.dial(ctx, k, addr, mac)
		q.mu.Lock()
		delete(q.dialing, k)
		close(done)
		if err != nil {
			q.failed[k] = time.Now()
			q.mu.Unlock()
			q.m.log.Debug("mesh QUIC 连不上，走 WireGuard", "peer", k.Short(), "err", err)
			return nil
		}
		q.mu.Unlock()
		q.register(k, c)
		return c
	}
}

// dial 建一条到 k 的 QUIC 连接并证明身份。
func (q *quicNet) dial(ctx context.Context, k Key, addr netip.AddrPort, mac [32]byte) (*quic.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, quicDialTimeout)
	defer cancel()
	c, err := q.tr.Dial(ctx, net.UDPAddrFromAddrPort(addr), q.tls, quicConfig())
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*quic.Conn, error) {
		_ = c.CloseWithError(1, "")
		return nil, err
	}
	q.useBBR(c)
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	mine, err := proof(mac, "client", c)
	if err != nil {
		return fail(err)
	}
	self := q.m.self
	msg := append(append(append([]byte{}, quicMagic...), self[:]...), mine...)
	_ = s.SetDeadline(time.Now().Add(quicDialTimeout))
	if _, err := s.Write(msg); err != nil {
		return fail(err)
	}
	theirs := make([]byte, proofLen)
	if _, err := io.ReadFull(s, theirs); err != nil {
		return fail(err)
	}
	want, _ := proof(mac, "server", c)
	if !hmac.Equal(theirs, want) {
		return fail(errors.New("对方的身份证明不对"))
	}
	_ = s.Close()
	return c, nil
}

// accept 接受别人建的 QUIC 连接。
func (q *quicNet) accept() {
	for {
		c, err := q.ln.Accept(context.Background())
		if err != nil {
			return
		}
		go q.handshake(c)
	}
}

// handshake 校验对方的身份证明，通过以后登记这条连接。
func (q *quicNet) handshake(c *quic.Conn) {
	ctx, cancel := context.WithTimeout(c.Context(), quicDialTimeout)
	defer cancel()
	fail := func() { _ = c.CloseWithError(1, "") }
	s, err := c.AcceptStream(ctx)
	if err != nil {
		fail()
		return
	}
	_ = s.SetDeadline(time.Now().Add(quicDialTimeout))
	msg := make([]byte, len(quicMagic)+32+proofLen)
	if _, err := io.ReadFull(s, msg); err != nil || !bytes.Equal(msg[:len(quicMagic)], quicMagic) {
		fail()
		return
	}
	var k Key
	copy(k[:], msg[len(quicMagic):])
	addr, mac, ok := q.peerFor(k)
	from, _ := netip.ParseAddrPort(c.RemoteAddr().String())
	if !ok || netip.AddrPortFrom(from.Addr().Unmap(), from.Port()) != addr {
		fail()
		return
	}
	want, err := proof(mac, "client", c)
	if err != nil || !hmac.Equal(msg[len(quicMagic)+32:], want) {
		fail()
		return
	}
	mine, _ := proof(mac, "server", c)
	if _, err := s.Write(mine); err != nil {
		fail()
		return
	}
	_ = s.Close()
	q.useBBR(c)
	q.register(k, c)
}

// register 登记到 k 的连接（已经有一条时留着旧的，新的也照样能收流），并开始接收对方开的流。
func (q *quicNet) register(k Key, c *quic.Conn) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		_ = c.CloseWithError(0, "")
		return
	}
	if q.conns[k] == nil {
		q.conns[k] = c
	}
	q.all[c] = k
	delete(q.failed, k)
	q.mu.Unlock()
	go func() {
		<-c.Context().Done()
		q.mu.Lock()
		if q.conns[k] == c {
			delete(q.conns, k)
		}
		delete(q.all, c)
		q.mu.Unlock()
	}()
	go q.serve(k, c)
}

// serve 接收对方在这条连接上开的流，按开头的端口交给对应的 Listen。
func (q *quicNet) serve(k Key, c *quic.Conn) {
	for {
		s, err := c.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func() {
			_ = s.SetReadDeadline(time.Now().Add(10 * time.Second))
			var hdr [2]byte
			if _, err := io.ReadFull(s, hdr[:]); err != nil {
				s.CancelRead(1)
				_ = s.Close()
				return
			}
			_ = s.SetReadDeadline(time.Time{})
			port := binary.BigEndian.Uint16(hdr[:])
			q.mu.Lock()
			l := q.listeners[port]
			q.mu.Unlock()
			if l == nil || !l.offer(q.wrap(s, k, port, false)) {
				s.CancelRead(1)
				s.CancelWrite(1)
			}
		}()
	}
}

// open 在到 k 的 QUIC 连接上开一个流，连 k 的 port 端口。
func (q *quicNet) open(ctx context.Context, k Key, port uint16) (net.Conn, error) {
	c := q.conn(ctx, k)
	if c == nil {
		return nil, errNoQUIC
	}
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], port)
	if _, err := s.Write(hdr[:]); err != nil {
		s.CancelRead(1)
		_ = s.Close()
		return nil, err
	}
	return q.wrap(s, k, port, true), nil
}

var errNoQUIC = errors.New("不能走 QUIC")

// txLoss 返回最近一个窗口里发给 k 的 QUIC 包的丢包率（百万分之几）和算出的时间，没算过时时间为零。
func (q *quicNet) txLoss(k Key) (uint32, time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.conns[k]
	l := q.loss[k]
	if c == nil {
		if l != nil {
			return l.ppm, l.at
		}
		return 0, time.Time{}
	}
	cc := q.counters[c]
	if cc == nil {
		return 0, time.Time{}
	}
	sentNow, lostNow := cc.sent.Load(), cc.lost.Load()
	if l == nil || l.conn != c {
		// 新连接：从连接建立时算起（计数本来就从 0 开始）；上一条连接算出的值留着。
		old := l
		l = &quicLoss{conn: c}
		if old != nil {
			l.ppm, l.at = old.ppm, old.at
		}
		q.loss[k] = l
	}
	if sent := sentNow - l.sent; sent >= quicLossSamples {
		lost := min(lostNow-l.lost, sent)
		l.ppm, l.at = uint32(lost*1_000_000/sent), time.Now()
		l.sent, l.lost = sentNow, lostNow
	}
	return l.ppm, l.at
}

// has 返回到 k 有没有建好的 QUIC 连接。
func (q *quicNet) has(k Key) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conns[k] != nil
}

// unfail 清掉到 k 连不上的记录，下次马上可以再试。
func (q *quicNet) unfail(k Key) {
	q.mu.Lock()
	delete(q.failed, k)
	q.mu.Unlock()
}

// closeAll 断开所有 QUIC 连接（关闭 QUIC 时）。
func (q *quicNet) closeAll() {
	q.mu.Lock()
	all := q.all
	q.conns, q.all = map[Key]*quic.Conn{}, map[*quic.Conn]Key{}
	q.failed = map[Key]time.Time{}
	q.mu.Unlock()
	for c := range all {
		_ = c.CloseWithError(0, "")
	}
}

// forget 关掉已经不在成员列表里的对端的连接。
func (q *quicNet) forget(keep map[Key]bool) {
	q.mu.Lock()
	var gone []*quic.Conn
	for k := range q.loss {
		if !keep[k] {
			delete(q.loss, k)
		}
	}
	for c, k := range q.all {
		if !keep[k] {
			gone = append(gone, c)
			delete(q.all, c)
			delete(q.conns, k)
		}
	}
	q.mu.Unlock()
	for _, c := range gone {
		_ = c.CloseWithError(0, "")
	}
}

// ---- 流包装成 net.Conn ----

// quicStream 是 QUIC 流包装的 TCP 连接：地址是双方的 mesh 地址（KeyOf 照常认人），CloseWrite 只关发送方向。
type quicStream struct {
	*quic.Stream
	local, remote *net.TCPAddr
}

func (q *quicNet) wrap(s *quic.Stream, k Key, port uint16, outgoing bool) *quicStream {
	self := &net.TCPAddr{IP: q.m.addr.AsSlice()}
	peer := &net.TCPAddr{IP: Address(k).AsSlice()}
	if outgoing {
		peer.Port = int(port)
	} else {
		self.Port = int(port)
	}
	return &quicStream{Stream: s, local: self, remote: peer}
}

func (s *quicStream) LocalAddr() net.Addr  { return s.local }
func (s *quicStream) RemoteAddr() net.Addr { return s.remote }
func (s *quicStream) CloseWrite() error    { return s.Stream.Close() }

func (s *quicStream) Close() error {
	s.CancelRead(0)
	return s.Stream.Close()
}

// ---- 两种连接都收的 Listener ----

// meshListener 同时接受 gVisor TCP 和 QUIC 流的连接。
type meshListener struct {
	tcp   net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	stop  func()
}

func newMeshListener(tcp net.Listener, stop func()) *meshListener {
	l := &meshListener{tcp: tcp, conns: make(chan net.Conn, 64), done: make(chan struct{}), stop: stop}
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				_ = l.Close()
				return
			}
			if !l.offer(c) {
				_ = c.Close()
			}
		}
	}()
	return l
}

// offer 交出一个连接，Listener 已经关了时返回 false。
func (l *meshListener) offer(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *meshListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *meshListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		_ = l.tcp.Close()
		if l.stop != nil {
			l.stop()
		}
	})
	return nil
}

func (l *meshListener) Addr() net.Addr { return l.tcp.Addr() }
