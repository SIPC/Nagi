package mesh

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// 已确认地址发来的空包要丢掉，不能读第一个字节时越界（UDP 来源地址能伪造）。
func TestReceiveEmptyUDP(t *testing.T) {
	b := newBind(Key{}, false, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	n, err := b.receiveUDP(c)(packets, sizes, eps)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || string(packets[0][:sizes[0]]) != string(want) || eps[0] != b.peers[k].ep {
		t.Fatalf("收到 %d 个包，大小 %d", n, sizes[0])
	}
}
