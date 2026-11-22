package server

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/server/auth"
)

// M6：握手失败（客户端发完 ClientHello 就断开、证书校验失败）时，指纹表里不能留下这个连接；
// 握手成功的连接指纹照常交给 HTTP 服务。
func TestHandshakeDropsFingerprint(t *testing.T) {
	ca, err := tlsutil.NewCA("nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := tlsutil.GenerateKey()
	leaf, err := ca.IssueServer(key, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"},
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			auth.RecordFingerprint(h)
			return nil, nil
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []*tls.Conn
	var fps []string
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c, fp, err := handshakeTLS(ctx, raw, cfg); err == nil {
					mu.Lock()
					ok, fps = append(ok, c), append(fps, fp)
					mu.Unlock()
				}
			}()
		}
	}()
	before := auth.FingerprintCount()
	for range 100 {
		// 证书校验失败：客户端在收到证书后中止握手。
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: "nagi-test"})
		if err == nil {
			c.Close()
		}
	}
	for range 100 {
		// 发完 ClientHello 就断开。
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
		go func() { _ = c.Handshake() }()
		time.Sleep(time.Millisecond)
		raw.Close()
	}
	good, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	// 等所有握手结束（服务端最多等 10 秒，客户端断开后很快返回）。
	time.Sleep(500 * time.Millisecond)
	ln.Close()
	wg.Wait()
	defer func() {
		for _, c := range ok {
			c.Close()
		}
	}()
	if n := auth.FingerprintCount() - before; n != 0 {
		t.Errorf("握手结束后指纹表里还剩 %d 项", n)
	}
	// 握手并发完成，按连接身份找到声明了 h2 的客户端，不能依赖完成顺序。
	i := slices.IndexFunc(ok, func(c *tls.Conn) bool {
		return c.RemoteAddr().String() == good.LocalAddr().String()
	})
	if i < 0 || !strings.Contains(fps[i], "h2_") {
		t.Fatalf("成功的握手应该带着指纹：%q", fps)
	}
	// 交给 HTTP 服务时按 TLS 连接放回去，ConnContext 取走后表里不留。
	auth.HoldFingerprint(ok[i], fps[i])
	if got := auth.TakeFingerprint(ok[i]); got != fps[i] || auth.FingerprintCount() != before {
		t.Errorf("取回的指纹 %q，表里 %d 项", got, auth.FingerprintCount())
	}
}
