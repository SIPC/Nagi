package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// proxyV2Sig 是 PROXY protocol v2 头的前 12 个字节。
var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// 打开 PROXY protocol 的 tcp、udp 映射转发给上游时，数据前面带着 v2 头；没打开的原样转发。
func TestProxyProtocol(t *testing.T) {
	tcpUp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpUp.Close()
	plainUp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plainUp.Close()
	udpUp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpUp.Close()
	tcpPort, plainPort, udpPort := freePort(t, "tcp"), freePort(t, "tcp"), freePort(t, "udp")

	routes := []Route{
		{Project: "p", Name: "mc", Protocol: "tcp", Published: tcpPort, Upstreams: []string{tcpUp.Addr().String()}, ProxyProtocol: true},
		{Project: "p", Name: "mc", Protocol: "udp", Published: udpPort, Upstreams: []string{udpUp.LocalAddr().String()}, ProxyProtocol: true},
		{Project: "p", Name: "db", Protocol: "tcp", Published: plainPort, Upstreams: []string{plainUp.Addr().String()}},
	}
	data, err := json.Marshal(build(routes, t.TempDir(), func(string) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(data, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()

	// TCP：上游先收到 v2 头，然后才是客户端发的数据。
	readTCP := func(ln net.Listener, port uint32) []byte {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(port)))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		up, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer up.Close()
		_ = up.SetReadDeadline(time.Now().Add(5 * time.Second))
		var got []byte
		buf := make([]byte, 256)
		for !bytes.HasSuffix(got, []byte("hello")) {
			n, err := up.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("read upstream: %v (got %q)", err, got)
			}
		}
		return got
	}
	if got := readTCP(tcpUp, tcpPort); !bytes.HasPrefix(got, proxyV2Sig) || !bytes.HasSuffix(got, []byte("hello")) {
		t.Fatalf("tcp with proxy protocol: got %q", got)
	}
	if got := readTCP(plainUp, plainPort); string(got) != "hello" {
		t.Fatalf("tcp without proxy protocol: got %q", got)
	}

	// UDP：每个数据报前面都带着头。
	c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(int(udpPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = udpUp.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	var n int
	for i := 0; i < 50; i++ {
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		_ = udpUp.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if n, _, err = udpUp.ReadFrom(buf); err == nil {
			break
		}
	}
	if !bytes.HasPrefix(buf[:n], proxyV2Sig) || !bytes.HasSuffix(buf[:n], []byte("ping")) {
		t.Fatalf("udp with proxy protocol: got %q", buf[:n])
	}
}

func freePort(t *testing.T, network string) uint32 {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return uint32(c.LocalAddr().(*net.UDPAddr).Port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint32(ln.Addr().(*net.TCPAddr).Port)
}
