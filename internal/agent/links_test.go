package agent

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
)

func TestParseLinkHeader(t *testing.T) {
	for _, c := range []struct {
		in    string
		alias string
		port  uint16
		ok    bool
	}{
		{"pg.base 5432", "pg.base", 5432, true},
		{"db.umami.base 3306", "db.umami.base", 3306, true},
		{"pg.base 0", "", 0, false},
		{"pg.base 70000", "", 0, false},
		{"pg.base", "", 0, false},
		{" 80", "", 0, false},
	} {
		alias, port, err := parseLinkHeader(c.in)
		if (err == nil) != c.ok || alias != c.alias || port != c.port {
			t.Errorf("%q: got %q %d %v", c.in, alias, port, err)
		}
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(strings.Repeat("a", 600) + "\n"))); err == nil {
		t.Error("太长的请求应该被拒绝")
	}
}

// TestSplice 检查转发时先读进缓冲的数据不会丢，一边关闭写方向后另一边还能把回答发完。
func TestSplice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 后端：读完请求再回答。
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		c.Write([]byte("echo:" + string(b)))
	}()
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l2.Close()
	go func() {
		c, err := l2.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		line, _ := readLine(br) // 头一行后面紧跟着的数据已经进了缓冲
		if line != "hdr" {
			c.Close()
			return
		}
		back, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			c.Close()
			return
		}
		splice(c, br, back, back)
		c.Close()
		back.Close()
	}()
	client, err := net.Dial("tcp", l2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client.Write([]byte("hdr\nhello"))
	client.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(client)
	if string(got) != "echo:hello" {
		t.Fatalf("got %q", got)
	}
}
