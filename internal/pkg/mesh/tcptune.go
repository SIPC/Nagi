package mesh

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// tcpTuning 是 mesh 里 gVisor TCP 的参数（见 docs/mesh.md#隧道里的-tcp）。
type tcpTuning struct {
	// cc 是拥塞控制："reno" 或 "cubic"。
	cc string
	// sendDefault、sendMax、recvDefault、recvMax 是每个连接的发送、接收缓冲：起始大小和自动调整的上限。
	// 缓冲只是上限，数据积压时才占内存。
	sendDefault, sendMax int
	recvDefault, recvMax int
	// moderate：按测出的带宽时延积自动调大接收缓冲（Linux 的 tcp_moderate_rcvbuf）。
	moderate bool
	// delay：Nagle（TCP_NODELAY 的反面）。gonet 的连接默认关着，这里是协议栈的默认值。
	delay bool
	// noRACK：不用 RACK 判断丢包，改用经典的三次重复 ACK。
	noRACK bool
}

// udpSocketBuffer 是 mesh 的 UDP 套接字的收发缓冲（字节），0 表示用系统默认（Linux 一般约 208 KiB）。测试里替换。
var udpSocketBuffer = 7 << 20

// gVisor 自己的默认值：reno，缓冲起始 1 MiB、最大 4 MiB，自动调整开着。
var gvisorDefaults = tcpTuning{cc: "reno", sendDefault: 1 << 20, sendMax: 4 << 20, recvDefault: 1 << 20, recvMax: 4 << 20, moderate: true}

// defaultTuning 是 mesh 用的参数：实测 cubic、更大的缓冲都没有更好（见 docs/mesh.md#隧道里的-tcp），保持 gVisor 的默认值，
// 在这里写明是为了和 wireguard-go 原来的设置一样打开 SACK、以后改参数有地方改。测试里替换。
var defaultTuning = gvisorDefaults

func (t tcpTuning) apply(s *stack.Stack) error {
	set := func(name string, opt tcpip.SettableTransportProtocolOption) error {
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, opt); err != nil {
			return fmt.Errorf("设置 TCP 参数 %s: %v", name, err)
		}
		return nil
	}
	sack := tcpip.TCPSACKEnabled(true)
	if err := set("sack", &sack); err != nil {
		return err
	}
	cc := tcpip.CongestionControlOption(t.cc)
	if err := set("congestion control", &cc); err != nil {
		return err
	}
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: t.sendDefault, Max: t.sendMax}
	if err := set("send buffer", &snd); err != nil {
		return err
	}
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: t.recvDefault, Max: t.recvMax}
	if err := set("receive buffer", &rcv); err != nil {
		return err
	}
	mod := tcpip.TCPModerateReceiveBufferOption(t.moderate)
	if err := set("moderate receive buffer", &mod); err != nil {
		return err
	}
	rec := tcpip.TCPRecovery(tcpip.TCPRACKLossDetection)
	if t.noRACK {
		rec = 0
	}
	if err := set("recovery", &rec); err != nil {
		return err
	}
	delay := tcpip.TCPDelayEnabled(t.delay)
	return set("delay", &delay)
}
