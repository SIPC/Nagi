package agent

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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

func TestCheckRemoteEndpoints(t *testing.T) {
	resp := &nagiv1.SharedNetworkInfoResponse{
		Projects: []*nagiv1.SharedNetworkProject{{NetworkName: "shop"}, {NetworkName: "base"}},
		Endpoints: []*nagiv1.LinkEndpoint{
			{Alias: "api.shop", Ports: []uint32{8080, 0, 70000}},
			{Alias: "redis.cache.shop", Ports: []uint32{6379}},
			{Alias: "pg.base", Ports: []uint32{5432}},  // base 被本节点的项目占用
			{Alias: "pg.other", Ports: []uint32{5432}}, // other 不是对方声明的网络名
			{Alias: "bad", Ports: []uint32{1}},
		},
	}
	got := checkRemoteEndpoints(resp, map[string]bool{"base": true})
	var names []string
	for _, e := range got {
		names = append(names, e.GetAlias())
	}
	if strings.Join(names, ",") != "api.shop,redis.cache.shop" {
		t.Fatalf("接受的容器 = %v", names)
	}
	if p := got[0].GetPorts(); len(p) != 1 || p[0] != 8080 {
		t.Fatalf("端口 = %v，应该去掉 0 和超范围的", p)
	}
}
