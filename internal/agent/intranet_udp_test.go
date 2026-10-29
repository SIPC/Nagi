package agent

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

// udpEcho 开一个 UDP 回显服务，回的内容前面加上客户端的来源端口，用来分辨每个流用的是不同的 socket。
func udpEcho(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2+udpMaxDatagram)
		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf[2:])
			if err != nil {
				return
			}
			binary.BigEndian.PutUint16(buf, from.Port())
			_, _ = pc.WriteToUDPAddrPort(buf[:2+n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

func flowMsg(id uint32, payload string) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, id)
	copy(b[4:], payload)
	return b
}

type udpReply struct {
	id   uint32
	port uint16
	data string
}

func TestUDPFlows(t *testing.T) {
	replies := make(chan udpReply, 16)
	f := newUDPFlows(udpEcho(t), func(msg []byte) {
		replies <- udpReply{binary.BigEndian.Uint32(msg), binary.BigEndian.Uint16(msg[4:]), string(msg[6:])}
	})
	defer f.close()
	recv := func() udpReply {
		t.Helper()
		select {
		case r := <-replies:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("没有收到回应")
			return udpReply{}
		}
	}

	f.send(flowMsg(1, "a"))
	r1 := recv()
	f.send(flowMsg(2, "b"))
	r2 := recv()
	f.send(flowMsg(1, "c"))
	r3 := recv()
	if r1.id != 1 || r1.data != "a" || r2.id != 2 || r2.data != "b" || r3.id != 1 || r3.data != "c" {
		t.Fatalf("回应和流对不上：%+v %+v %+v", r1, r2, r3)
	}
	// 每个流一个 socket：同一个流来源端口不变，不同的流不同。
	if r1.port != r3.port || r1.port == r2.port {
		t.Fatalf("来源端口不对：%d %d %d", r1.port, r2.port, r3.port)
	}

	// 满了以后关掉最久没动的流。
	f.mu.Lock()
	n := len(f.flows)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("应该有 2 个流，实际 %d", n)
	}
	f.mu.Lock()
	f.flows[2].last.Store(time.Now().Add(-time.Minute).UnixNano())
	f.evictLocked()
	_, has1 := f.flows[1]
	_, has2 := f.flows[2]
	f.mu.Unlock()
	if !has1 || has2 {
		t.Fatalf("应该关掉流 2：%v %v", has1, has2)
	}

	f.close()
	select {
	case <-f.done():
	default:
		t.Fatal("关闭后 done 应该关闭")
	}
	f.send(flowMsg(3, "x")) // 关闭后发送不应该出问题
}

func TestUDPIdle(t *testing.T) {
	if udpIdleFor(0) != udpShortIdle || udpIdleFor(1) != udpShortIdle || udpIdleFor(2) != udpIdle {
		t.Fatal("空闲超时不对")
	}
}
