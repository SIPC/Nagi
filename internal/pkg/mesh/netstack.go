package mesh

// mesh 里的 TCP/IP 栈（gVisor netstack），WireGuard 解密后的 IP 包交给它，它发出的 IP 包交给 WireGuard 加密。
// 改自 golang.zx2c4.com/wireguard/tun/netstack（MIT 许可，Copyright (C) 2017-2025 WireGuard LLC），
// 只留下 mesh 用到的部分；TCP 的参数在 tcptune.go，取舍见 docs/mesh.md#隧道里的-tcp。

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"syscall"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	ipheader "gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// netTUN 是给 WireGuard 用的虚拟网卡，背后是 gVisor 的协议栈。
type netTUN struct {
	ep             *channel.Endpoint
	stack          *stack.Stack
	events         chan tun.Event
	notifyHandle   *channel.NotificationHandle
	incomingPacket chan *buffer.View
	mtu            int
}

// stackQueue 是协议栈发往 WireGuard 的包的队列长度（channel.Endpoint 满了就丢）；
// tunQueue 是交给 WireGuard 之前再排的队（满了让协议栈等，不丢）。
const (
	stackQueue = 1024
	tunQueue   = 256
)

// tunBatch：Read 一次交多个包。测试里关掉对比。
var tunBatch = true

func newNetTUN(addr netip.Addr, mtu int, t tcpTuning) (*netTUN, error) {
	opts := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6, icmp.NewProtocol4},
		HandleLocal:        true,
	}
	dev := &netTUN{
		ep:             channel.New(stackQueue, uint32(mtu), ""),
		stack:          stack.New(opts),
		events:         make(chan tun.Event, 10),
		incomingPacket: make(chan *buffer.View, tunQueue),
		mtu:            mtu,
	}
	if err := t.apply(dev.stack); err != nil {
		dev.stack.Close()
		return nil, err
	}
	dev.notifyHandle = dev.ep.AddNotify(dev)
	if err := dev.stack.CreateNIC(1, dev.ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %v", err)
	}
	pn := tcpip.NetworkProtocolNumber(ipv6.ProtocolNumber)
	route := ipheader.IPv6EmptySubnet
	if addr.Is4() {
		pn, route = ipv4.ProtocolNumber, ipheader.IPv4EmptySubnet
	}
	pa := tcpip.ProtocolAddress{Protocol: pn, AddressWithPrefix: tcpip.AddrFromSlice(addr.AsSlice()).WithPrefix()}
	if err := dev.stack.AddProtocolAddress(1, pa, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("AddProtocolAddress(%v): %v", addr, err)
	}
	dev.stack.AddRoute(tcpip.Route{Destination: route, NIC: 1})
	dev.events <- tun.EventUp
	return dev, nil
}

func (t *netTUN) Name() (string, error)    { return "go", nil }
func (t *netTUN) File() *os.File           { return nil }
func (t *netTUN) Events() <-chan tun.Event { return t.events }
func (t *netTUN) MTU() (int, error)        { return t.mtu, nil }
func (t *netTUN) BatchSize() int           { return 1 }

// Read 把协议栈发出的包交给 WireGuard：等到第一个，再把已经排着的一起交（最多 len(buf) 个）。
// 一次只交一个时，协议栈每发一个包都要等 WireGuard 的读取协程醒来接手，线路有延迟、包一阵一阵来时
// 协程总在睡和醒之间切换，隧道里的 TCP 只能跑到几 MiB/s（见 docs/mesh.md#隧道里的-tcp）。
func (t *netTUN) Read(buf [][]byte, sizes []int, offset int) (int, error) {
	view, ok := <-t.incomingPacket
	if !ok {
		return 0, os.ErrClosed
	}
	n := 0
	for {
		size, err := view.Read(buf[n][offset:])
		view.Release()
		if err != nil {
			return n, err
		}
		sizes[n] = size
		n++
		if n == len(buf) || !tunBatch {
			return n, nil
		}
		select {
		case view, ok = <-t.incomingPacket:
			if !ok {
				return n, nil
			}
		default:
			return n, nil
		}
	}
}

// Write 把 WireGuard 解密出的包交给协议栈。
func (t *netTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		packet := b[offset:]
		if len(packet) == 0 {
			continue
		}
		pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		switch packet[0] >> 4 {
		case 4:
			t.ep.InjectInbound(ipheader.IPv4ProtocolNumber, pkb)
		case 6:
			t.ep.InjectInbound(ipheader.IPv6ProtocolNumber, pkb)
		default:
			pkb.DecRef()
			return 0, syscall.EAFNOSUPPORT
		}
		pkb.DecRef()
	}
	return len(bufs), nil
}

// WriteNotify：协议栈有包要发。
func (t *netTUN) WriteNotify() {
	pkt := t.ep.Read()
	if pkt == nil {
		return
	}
	view := pkt.ToView()
	pkt.DecRef()
	t.incomingPacket <- view
}

func (t *netTUN) Close() error {
	t.stack.RemoveNIC(1)
	t.stack.Close()
	t.ep.RemoveNotify(t.notifyHandle)
	t.ep.Close()
	if t.events != nil {
		close(t.events)
	}
	if t.incomingPacket != nil {
		close(t.incomingPacket)
	}
	return nil
}

func fullAddr(ap netip.AddrPort) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	pn := tcpip.NetworkProtocolNumber(ipv6.ProtocolNumber)
	if ap.Addr().Is4() {
		pn = ipv4.ProtocolNumber
	}
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}, pn
}

func (t *netTUN) dialTCP(ctx context.Context, ap netip.AddrPort) (*gonet.TCPConn, error) {
	fa, pn := fullAddr(ap)
	return gonet.DialContextTCP(ctx, t.stack, fa, pn)
}

func (t *netTUN) listenTCP(ap netip.AddrPort) (*gonet.TCPListener, error) {
	fa, pn := fullAddr(ap)
	return gonet.ListenTCP(t.stack, fa, pn)
}

// dialUDP：raddr 无效时只绑定本地地址（监听）。
func (t *netTUN) dialUDP(laddr, raddr netip.AddrPort) (*gonet.UDPConn, error) {
	var lfa, rfa *tcpip.FullAddress
	var pn tcpip.NetworkProtocolNumber
	if laddr.IsValid() {
		a, p := fullAddr(laddr)
		lfa, pn = &a, p
	}
	if raddr.IsValid() {
		a, p := fullAddr(raddr)
		rfa, pn = &a, p
	}
	return gonet.DialUDP(t.stack, lfa, rfa, pn)
}
