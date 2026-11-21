//go:build linux

package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 不等 OK 先发数据：A 经真的 mesh 连 B、C。B、C 上是假的目标节点：
//   - B 读完请求后先看客户端的数据是不是已经到了（没等 OK 就发了），然后回"不在这里"；
//   - C 收到请求和同样的数据（A 重发的），回 OK 后把数据加上标记回过去。
func TestTCPEarlyDataAndRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	newMesh := func() *mesh.Mesh {
		var priv mesh.Key
		_, _ = rand.Read(priv[:])
		m, err := mesh.New(mesh.Config{PrivateKey: priv, Logger: log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	mA, mB, mC := newMesh(), newMesh(), newMesh()
	all := []*mesh.Mesh{mA, mB, mC}
	for i, x := range all {
		var peers []mesh.Peer
		for j, y := range all {
			if i != j {
				peers = append(peers, mesh.Peer{Key: y.PublicKey(), Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), y.Port())}})
			}
		}
		if err := x.SetPeers(peers); err != nil {
			t.Fatal(err)
		}
	}

	type seen struct {
		header    string
		early     string // OK（或拒绝）之前已经收到的数据
		earlyRead bool
	}
	// fake 在 m 的 7001 端口上当目标节点：读请求行，等最多 2 秒看客户端的数据（不回 OK 就到了才算先发），
	// accept 为 false 时回"不在这里"，否则回 OK 再回 "C:" 加收到的全部数据。
	fake := func(m *mesh.Mesh, accept bool, out chan<- seen) {
		ln, err := m.Listen(linkPort)
		if err != nil {
			t.Error(err)
			return
		}
		go func() { <-ctx.Done(); ln.Close() }()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := readLine(br)
				if err != nil {
					return
				}
				s := seen{header: line}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 64)
				if k, _ := io.ReadAtLeast(br, buf, len("hello\n")); k > 0 {
					s.early, s.earlyRead = string(buf[:k]), true
				}
				_ = c.SetReadDeadline(time.Time{})
				out <- s
				if !accept {
					_, _ = io.WriteString(c, "ERR 不在这里\n")
					return
				}
				rest, _ := io.ReadAll(br) // A 在客户端关了写方向以后也关掉这条流的写方向
				_, _ = io.WriteString(c, "OK\nC:"+s.early+string(rest))
			}(c)
		}
	}
	seenB, seenC := make(chan seen, 4), make(chan seen, 4)
	go fake(mB, false, seenB)
	go fake(mC, true, seenC)

	a := &agent{log: log, ctx: ctx, mesh: &meshNode{m: mA}}
	store, err := logstore.Open(ctx, filepath.Join(t.TempDir(), "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a.cfg.Logs = store
	n := &intranet{a: a}
	a.intranet = n
	kB, kC := mB.PublicKey(), mC.PublicKey()
	p := &nagiv1.LinkProxy{ProjectId: testProject, Alias: "web", TargetProject: testProject, TargetName: "web", Ports: []uint32{80},
		Nodes: []*nagiv1.LinkNode{{Key: kB[:], Name: "B"}, {Key: kC[:], Name: "C"}}}

	// 客户端：一连上就发请求、关写方向，读回应。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		n.forward(c, c, p, 80)
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := io.WriteString(client, "hello\n"); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "C:hello\n" {
		t.Fatalf("回应：%q", got)
	}
	select {
	case <-forwarded:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
	}
	events, err := store.Query(ctx, logstore.Filter{Category: logstore.Access})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, ev := range events {
		if ev.Kind != "intranet.tcp.attempt" {
			continue
		}
		b, _ := json.Marshal(ev.Data)
		var data struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, data.Outcome)
	}
	if strings.Join(outcomes, ",") != "target_rejected,accepted" {
		t.Fatalf("persisted attempts: %v", outcomes)
	}
	for name, ch := range map[string]chan seen{"B": seenB, "C": seenC} {
		select {
		case s := <-ch:
			if !strings.HasSuffix(s.header, " web 80") {
				t.Errorf("%s 收到的请求：%q", name, s.header)
			}
			if !s.earlyRead || s.early != "hello\n" {
				t.Errorf("%s 回复之前没有收到客户端的数据（没有先发）：%+v", name, s)
			}
		default:
			t.Errorf("%s 没有收到请求", name)
		}
	}
}

// dialRemote（测试用）：不带客户端数据，只连到目标节点，返回对方回 OK 以后的连接。
func (n *intranet) dialRemote(k mesh.Key, name string, p *nagiv1.LinkProxy, port uint16) (net.Conn, *bufio.Reader, bool) {
	pump := &clientPump{chunks: make(chan []byte), done: make(chan struct{})} // 客户端一直没有数据
	var early pending
	return n.tryRemote(k, name, p, port, pump, &early)
}
