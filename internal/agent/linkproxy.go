package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// RunLinkProxy 是代理容器里运行的程序（nagi link-proxy，见 links.go）：在环境变量 NAGI_LINK_PORTS
// 列出的每个 TCP 端口上监听，每个连接都经 /run/nagi/sock 交给节点，第一行是"名字 端口"。
func RunLinkProxy() error {
	alias := os.Getenv("NAGI_LINK_ALIAS")
	if alias == "" {
		return errors.New("缺少 NAGI_LINK_ALIAS")
	}
	for _, s := range strings.Split(os.Getenv("NAGI_LINK_PORTS"), ",") {
		if s == "" {
			continue
		}
		p, err := strconv.ParseUint(s, 10, 16)
		if err != nil || p == 0 {
			return fmt.Errorf("端口 %q 不对", s)
		}
		ln, err := net.Listen("tcp", ":"+s)
		if err != nil {
			return err
		}
		go func(ln net.Listener, port uint64) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go linkProxyConn(c, alias, port)
			}
		}(ln, p)
	}
	fmt.Printf("代理 %s，端口 %s\n", alias, os.Getenv("NAGI_LINK_PORTS"))
	select {}
}

func linkProxyConn(c net.Conn, alias string, port uint64) {
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
