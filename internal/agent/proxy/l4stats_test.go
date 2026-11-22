package proxy

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/caddyserver/caddy/v2"
)

// tcp/udp 映射经 nagi_stats 统计：新连接数、收发字节、当前连接数、连不上容器的次数；UDP 按会话算连接。
// 容器先关闭连接时，客户端照常收到 EOF（包装后的连接要把 CloseWrite 转给底层连接）。
func TestL4Stats(t *testing.T) {
	TakeStats()
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	OnConnection(func(e ConnectionRecord) {
		logs.Report(logstore.Event{Time: e.Time, Category: logstore.Access, Kind: "proxy.l4." + e.Phase, Data: e})
	})
	defer OnConnection(nil)
	tcpUp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpUp.Close()
	udpUp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpUp.Close()
	// 没人监听的端口：连不上容器。
	dead := freePort(t, "tcp")
	tcpPort, udpPort, deadPort := freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "tcp")
	for deadPort == dead || deadPort == tcpPort {
		deadPort = freePort(t, "tcp")
	}

	// Caddy may finish an old UDP session after Stop returns. Scope the
	// process-wide route statistics to this test's own project.
	project := rand.Text()
	routes := []Route{
		{Project: project, Name: "game", Protocol: "tcp", Published: tcpPort, Upstreams: []string{tcpUp.Addr().String()}},
		{Project: project, Name: "game", Protocol: "udp", Published: udpPort, Upstreams: []string{udpUp.LocalAddr().String()}},
		{Project: project, Name: "down", Protocol: "tcp", Published: deadPort, Upstreams: []string{"127.0.0.1:" + strconv.Itoa(int(dead))}},
	}
	data, err := json.Marshal(build(routes, t.TempDir(), func(string) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(data, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()

	// TCP：客户端发 5 个字节，容器回 3 个字节后关闭连接。
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(tcpPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	up, err := tcpUp.Accept()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_ = up.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(up, buf[:5]); err != nil {
		t.Fatal(err)
	}
	// 连接还开着：当前连接数是 1。
	if s := TakeStats()[project+"/game"]; !s.L4 || s.Active != 1 || s.Conns != 1 || s.BytesIn != 5 {
		t.Fatalf("连接开着时：%+v", s)
	}
	if _, err := up.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	_ = up.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "bye" {
		t.Fatalf("容器关闭后客户端应该读到 bye 和 EOF：%q %v", got, err)
	}
	_ = c.Close()

	// UDP：一个会话，收发各 4 个字节。
	u, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(int(udpPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	var from net.Addr
	for range 50 {
		if _, err := u.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		_ = udpUp.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		var n int
		if n, from, err = udpUp.ReadFrom(buf); err == nil && string(buf[:n]) == "ping" {
			break
		}
	}
	if from == nil {
		t.Fatal("udp 上游没收到数据")
	}
	if _, err := udpUp.WriteTo([]byte("pong"), from); err != nil {
		t.Fatal(err)
	}
	_ = u.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("udp 客户端应该收到 pong：%q %v", buf[:n], err)
	}

	// 连不上容器的映射：记一个连接、一次失败。
	d, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(deadPort)))
	if err != nil {
		t.Fatal(err)
	}
	_ = d.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.ReadAll(d)
	_ = d.Close()

	// tcp 连接已经结束，udp 会话还在（空闲 30 秒才结束），两条同名规则记在一起。
	var game, down RouteStats
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := TakeStats()
		g, dn := s[project+"/game"], s[project+"/down"]
		game.Conns += g.Conns
		game.BytesIn += g.BytesIn
		game.BytesOut += g.BytesOut
		game.Active = g.Active
		down.Conns += dn.Conns
		down.Fails += dn.Fails
		if game.Conns >= 1 && game.BytesOut >= 7 && down.Fails >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 上面已经取走了 tcp 连接的 1 个连接和 5 个字节，这里是 udp 的 1 个会话和之后的字节。
	// udp 的 ping 在 Caddy 刚启动时可能重发了几次。
	if game.Conns != 1 || game.BytesIn < 4 || game.BytesIn%4 != 0 || game.BytesOut != 3+4 || game.Active != 1 {
		t.Fatalf("game：%+v", game)
	}
	if down.Conns != 1 || down.Fails != 1 {
		t.Fatalf("down：%+v", down)
	}
	// Read back actual durable ingress events, not callback invocations. The
	// still-open UDP session must not invent a close event or HTTP status.
	var events []logstore.Event
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		events, err = logs.Query(t.Context(), logstore.Filter{Category: logstore.Access})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) != 5 {
		t.Fatalf("want 3 opens and 2 closes, got %+v", events)
	}
	opens := map[string]ConnectionRecord{}
	closed, udpOpen, rejected := false, false, false
	for _, event := range events {
		var e ConnectionRecord
		if err := json.Unmarshal(event.Data.(json.RawMessage), &e); err != nil {
			t.Fatal(err)
		}
		if e.ID == "" || e.Local == "" || e.Remote == "" {
			t.Fatalf("missing identity/address: %+v", e)
		}
		if e.Phase == "open" {
			opens[e.ID] = e
			if e.Protocol == "udp" {
				udpOpen = true
			}
			continue
		}
		start, ok := opens[e.ID]
		if !ok || !e.Opened.Equal(start.Time) || e.Duration < 0 || e.Time.Before(start.Time) {
			t.Fatalf("unmatched end %+v", e)
		}
		if e.Route == project+"/game" && e.Protocol == "tcp" {
			closed = e.BytesIn == 5 && e.BytesOut == 3 && e.EndReason == "observer_finished" && e.Error == ""
		}
		if e.Route == project+"/down" {
			rejected = e.EndReason == "handler_error" && e.Error != ""
		}
	}
	if !closed || !udpOpen || !rejected {
		t.Fatalf("missing TCP bytes/UDP open/upstream failure: %v %v %v", closed, udpOpen, rejected)
	}
}
