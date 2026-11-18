//go:build !linux

package mesh

import "net"

// tuneUDP 见 sockbuf_linux.go。其他系统只按系统允许的上限设。
func tuneUDP(c *net.UDPConn) {
	if udpSocketBuffer <= 0 {
		return
	}
	_ = c.SetReadBuffer(udpSocketBuffer)
	_ = c.SetWriteBuffer(udpSocketBuffer)
}
