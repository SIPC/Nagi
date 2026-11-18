package mesh

import (
	"net"

	"golang.org/x/sys/unix"
)

// tuneUDP 把 UDP 套接字的收发缓冲设成 udpSocketBuffer：先按 net.core.{r,w}mem_max 设，
// 有 CAP_NET_ADMIN（节点以 root 运行）时用 SO_*BUFFORCE 越过上限（和 wireguard-go 的做法一样）。
// 失败不影响使用，只是线路延迟高、一阵一阵来包时更容易在套接字上丢包。
func tuneUDP(c *net.UDPConn) {
	if udpSocketBuffer <= 0 {
		return
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, udpSocketBuffer)
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, udpSocketBuffer)
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, udpSocketBuffer)
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, udpSocketBuffer)
	})
}
