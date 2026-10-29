package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunLinkProxy 是代理容器里运行的程序（nagi link-proxy，见 links.go）：在环境变量 NAGI_LINK_PORTS
// 列出的每个 TCP 端口上监听，每个连接都经 /run/nagi/sock 交给节点，第一行是"名字 端口"；
// NAGI_LINK_UDP_PORTS 列出的每个 UDP 端口经 /run/nagi/udp 交给节点（见 intranet_udp.go）。
func RunLinkProxy() error {
	alias := os.Getenv("NAGI_LINK_ALIAS")
	if alias == "" {
		return errors.New("缺少 NAGI_LINK_ALIAS")
	}
	tcpPorts, err := parsePortList(os.Getenv("NAGI_LINK_PORTS"))
	if err != nil {
		return err
	}
	udpPorts, err := parsePortList(os.Getenv("NAGI_LINK_UDP_PORTS"))
	if err != nil {
		return err
	}
	for _, p := range tcpPorts {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(int(p)))
		if err != nil {
			return err
		}
		go func(ln net.Listener, port uint16) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go linkProxyConn(c, alias, port)
			}
		}(ln, p)
	}
	for _, p := range udpPorts {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(p)})
		if err != nil {
			return err
		}
		go newUDPProxy(pc, "/run/nagi/udp", alias, p).run()
	}
	fmt.Printf("代理 %s，TCP 端口 %s，UDP 端口 %s\n", alias, os.Getenv("NAGI_LINK_PORTS"), os.Getenv("NAGI_LINK_UDP_PORTS"))
	select {}
}

func parsePortList(s string) ([]uint16, error) {
	var out []uint16
	for _, x := range strings.Split(s, ",") {
		if x == "" {
			continue
		}
		p, err := strconv.ParseUint(x, 10, 16)
		if err != nil || p == 0 {
			return nil, fmt.Errorf("端口 %q 不对", x)
		}
		out = append(out, uint16(p))
	}
	return out, nil
}

func linkProxyConn(c net.Conn, alias string, port uint16) {
	defer c.Close()
	u, err := net.Dial("unix", "/run/nagi/sock")
	if err != nil {
		return
	}
	defer u.Close()
	if _, err := fmt.Fprintf(u, "%s %d\n", alias, port); err != nil {
		return
	}
	splice(c, c, u, u)
}

// splice 在两个连接之间双向转发，一边写完后关闭对方的写方向，两边都结束才返回。
func splice(a net.Conn, ar io.Reader, b net.Conn, br io.Reader) {
	done := make(chan struct{}, 2)
	pipe := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(b, ar)
	go pipe(a, br)
	<-done
	<-done
}

// ---- UDP ----

// udpMaxClients 是代理一个 UDP 端口最多同时记住的客户端，满了忘掉最久没动的。
const udpMaxClients = 4096

// udpProxy 是代理容器的一个 UDP 端口：按客户端地址分配流 ID，数据报前面加上流 ID 经 unixpacket 交给节点，
// 节点回的数据报按流 ID 发回给对应的客户端。
type udpProxy struct {
	pc    *net.UDPConn
	sock  string // 节点的 unixpacket socket
	alias string
	port  uint16

	mu      sync.Mutex
	up      net.Conn // 到节点的连接，断开时为 nil（期间的数据报丢掉）
	clients map[netip.AddrPort]*udpClient
	byID    map[uint32]*udpClient
	next    uint32
}

func newUDPProxy(pc *net.UDPConn, sock, alias string, port uint16) *udpProxy {
	return &udpProxy{pc: pc, sock: sock, alias: alias, port: port, clients: map[netip.AddrPort]*udpClient{}, byID: map[uint32]*udpClient{}}
}

type udpClient struct {
	id   uint32
	addr netip.AddrPort
	last time.Time
	sent int64
}

func (u *udpProxy) run() {
	go u.connect()
	go u.expire()
	buf := make([]byte, udpBufSize)
	for {
		n, addr, err := u.pc.ReadFromUDPAddrPort(buf[4:])
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n > udpMaxDatagram {
			continue
		}
		u.mu.Lock()
		up := u.up
		var id uint32
		if up != nil {
			c := u.client(addr)
			c.last = time.Now()
			if c.sent < 2 {
				c.sent++
			}
			id = c.id
		}
		u.mu.Unlock()
		if up == nil {
			continue
		}
		binary.BigEndian.PutUint32(buf, id)
		_, _ = up.Write(buf[:4+n])
	}
}

// client 返回客户端的记录，没有就分配一个新的流 ID。流 ID 不重复使用，节点那边不会把新客户端当成旧的。调用方持有 u.mu。
func (u *udpProxy) client(addr netip.AddrPort) *udpClient {
	if c := u.clients[addr]; c != nil {
		return c
	}
	if len(u.clients) >= udpMaxClients {
		var old *udpClient
		for _, c := range u.clients {
			if old == nil || c.last.Before(old.last) {
				old = c
			}
		}
		u.forget(old)
	}
	u.next++
	c := &udpClient{id: u.next, addr: addr}
	u.clients[addr], u.byID[c.id] = c, c
	return c
}

func (u *udpProxy) forget(c *udpClient) {
	delete(u.clients, c.addr)
	delete(u.byID, c.id)
}

// connect 维持到节点的 unixpacket 连接：第一条消息是"名字 端口"，之后收节点回的数据报。断了 udpReconnect 后重连。
func (u *udpProxy) connect() {
	for {
		c, err := net.Dial("unixpacket", u.sock)
		if err == nil {
			if _, err := fmt.Fprintf(c, "%s %d\n", u.alias, u.port); err == nil {
				u.mu.Lock()
				u.up = c
				u.mu.Unlock()
				u.recv(c)
				u.mu.Lock()
				u.up = nil
				u.mu.Unlock()
			}
			c.Close()
		}
		time.Sleep(udpReconnect)
	}
}

func (u *udpProxy) recv(c net.Conn) {
	buf := make([]byte, udpBufSize)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		if n < 4 || n > 4+udpMaxDatagram {
			continue
		}
		u.mu.Lock()
		cl := u.byID[binary.BigEndian.Uint32(buf)]
		var addr netip.AddrPort
		if cl != nil {
			cl.last = time.Now()
			addr = cl.addr
		}
		u.mu.Unlock()
		if cl != nil {
			_, _ = u.pc.WriteToUDPAddrPort(buf[4:n], addr)
		}
	}
}

// expire 定时忘掉空闲的客户端，超时和节点那边的流一样。
func (u *udpProxy) expire() {
	for range time.Tick(10 * time.Second) {
		now := time.Now()
		u.mu.Lock()
		for _, c := range u.clients {
			if now.Sub(c.last) >= udpIdleFor(c.sent) {
				u.forget(c)
			}
		}
		u.mu.Unlock()
	}
}
