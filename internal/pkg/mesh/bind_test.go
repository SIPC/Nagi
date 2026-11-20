package mesh

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

func TestPingSourceFloodIsBounded(t *testing.T) {
	b := newBind(Key{9}, false, 0, 0, slog.Default())
	k := PublicKey(Key{19})
	b.setPeers([]Peer{{Key: k}})
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := b.peers[k]
	for i := 0; i < 1000; i++ {
		if i%64 == 0 {
			p.pingWindow = time.Time{}
		}
		b.handleControl(c, 0, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(10000+i)), encodePing(k, [txidLen]byte{}, p.mac))
	}
	if len(p.learned) > 32 || len(p.lastPing) > 512 || len(p.pings) > 512 {
		t.Fatalf("unbounded ping state: %d %d %d", len(p.learned), len(p.lastPing), len(p.pings))
	}
}

func TestSelfRelayDuringPeerRemoval(t *testing.T) {
	b := newBind(Key{9}, true, 0, 0, slog.Default())
	k := PublicKey(Key{19})
	from := netip.MustParseAddrPort("127.0.0.1:10000")
	p := &peerState{key: k, ep: &endpoint{key: k}, direct: from, directAt: time.Now()}
	pkt := encodeRelay(typeRelaySend, b.pub, []byte("payload"))
	b.peers[k], b.byAddr[from] = p, k
	if ep, data := b.handleControl(nil, 0, from, pkt); ep != p.ep || string(data) != "payload" {
		t.Fatal("self relay did not deliver")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20000 {
			b.mu.Lock()
			b.peers[k], b.byAddr[from] = p, k
			b.mu.Unlock()
			b.mu.Lock()
			delete(b.peers, k)
			delete(b.byAddr, from)
			b.mu.Unlock()
		}
	}()
	for range 20000 {
		ep, data := b.handleControl(nil, 0, from, pkt)
		if ep != nil && (ep != p.ep || string(data) != "payload") {
			t.Errorf("incorrect delivery: %v %q", ep, data)
			break
		}
	}
	wg.Wait()
	if ep, _ := b.handleControl(nil, 0, from, pkt); ep != nil {
		t.Fatal("removed peer accepted")
	}
}

// 已确认地址发来的空包要丢掉，不能读第一个字节时越界（UDP 来源地址能伪造）。
func TestReceiveEmptyUDP(t *testing.T) {
	b := newBind(Key{}, false, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := net.DialUDP("udp4", nil, c.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	k := PublicKey(Key{1})
	b.peers[k] = &peerState{key: k, ep: &endpoint{key: k}}
	b.byAddr[s.LocalAddr().(*net.UDPAddr).AddrPort()] = k

	if _, err := s.Write(nil); err != nil {
		t.Fatal(err)
	}
	want := []byte{4, 0, 0, 0} // WireGuard 数据包
	if _, err := s.Write(want); err != nil {
		t.Fatal(err)
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	packets, sizes, eps := [][]byte{make([]byte, 1500)}, []int{0}, []conn.Endpoint{nil}
	n, err := b.receiveUDP(c, 0)(packets, sizes, eps)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || string(packets[0][:sizes[0]]) != string(want) || eps[0] != b.peers[k].ep {
		t.Fatalf("收到 %d 个包，大小 %d", n, sizes[0])
	}
}

// 低阶点算出的共享密钥是全零，谁都算得出来，这样的公钥不能用。
func TestLowOrderPublicKey(t *testing.T) {
	bad := []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	}
	for _, h := range bad {
		raw, _ := hex.DecodeString(h)
		k, err := ParseKey(raw)
		if err != nil {
			t.Fatal(err)
		}
		if ValidPublicKey(k) {
			t.Fatalf("%s 应该无效", h)
		}
		if _, ok := macKey(Key{1}, k); ok {
			t.Fatalf("%s 不能算出 MAC 密钥", h)
		}
	}
	if !ValidPublicKey(PublicKey(Key{1})) {
		t.Fatal("正常的公钥应该有效")
	}

	m, err := New(Config{PrivateKey: Key{8}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	good := PublicKey(Key{16}) // 私钥低 3 位会被清掉，Key{1} 和 Key{2} 是同一个
	if err := m.SetPeers([]Peer{{Key: Key{}}, {Key: good}}); err != nil {
		t.Fatal(err)
	}
	m.bind.mu.Lock()
	n, ok := len(m.bind.peers), m.bind.peers[good] != nil
	m.bind.mu.Unlock()
	m.mu.Lock()
	wg := len(m.configured) == 1 && m.configured[good]
	m.mu.Unlock()
	if n != 1 || !ok || !wg {
		t.Fatal("应该只加入有效的成员")
	}
}
