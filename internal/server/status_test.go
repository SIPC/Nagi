package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
)

// L17：配过备用面板的面板，开了安全入口以后 /api/standby/status 仍然返回带证书指纹的 JSON，扫描的人能认出是面板。
// 现在开了安全入口时只回答出示了同一组面板 CA 签发的证书的调用方；同一组的面板（FetchStatus）照常拿到。
func TestStandbyStatusNeedsGroupCert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q := store.New(d)
	key, _ := sealer.Load(filepath.Join(dir, "master.key"))
	ca, err := tlsutil.NewCA("nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(c *tlsutil.CA) tls.Certificate {
		k, _ := tlsutil.GenerateKey()
		leaf, err := c.IssueServer(k, []string{"127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{leaf.Raw, c.Cert.Raw}, PrivateKey: k, Leaf: leaf}
	}
	s := &Server{q: q, ca: ca, cert: issue(ca), auth: auth.New(q, key)}
	s.fingerprint = tlsutil.Fingerprint(s.cert.Leaf)
	// 配过备用面板：任期大于 1。
	if err := q.SetSetting(ctx, store.SetSettingParams{Key: standby.SettingTerm, Value: "3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.auth.SetEntrance(ctx, true); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 和 acceptLoop 一样按 ALPN 分流：同一组的面板走 serveGroupStatus，其余交给 HTTP 服务。
	httpLn := newChanListener(ln.Addr())
	srv := &http.Server{Handler: http.HandlerFunc(s.handleStatus)}
	go func() { _ = srv.Serve(httpLn) }()
	defer srv.Close()
	cfg := s.tlsConfig()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, _, err := handshakeTLS(ctx, raw, cfg)
				if err != nil {
					return
				}
				if conn.ConnectionState().NegotiatedProtocol == standby.StatusALPN {
					s.serveGroupStatus(ctx, conn)
					return
				}
				if !httpLn.put(conn) {
					conn.Close()
				}
			}()
		}
	}()
	addr := ln.Addr().String()

	// 扫描的人：浏览器式的请求，或者自己生成一张证书。
	scan := func(cert *tls.Certificate, protos []string) int {
		cfg := &tls.Config{InsecureSkipVerify: true, NextProtos: protos}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		res, err := c.Get("https://" + addr + "/api/standby/status")
		if err != nil {
			return 0
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if code := scan(nil, []string{"http/1.1"}); code != http.StatusNotFound {
		t.Fatalf("开了安全入口时，没有证书的请求应该是 404，实际 %d", code)
	}
	other, _ := tlsutil.NewCA("other")
	fake := issue(other)
	if code := scan(&fake, []string{standby.StatusALPN}); code == http.StatusOK {
		t.Fatal("别的 CA 签发的证书不应拿到状态")
	}

	// 同一组的面板（另一个面板用自己的服务端密钥签客户端证书）。
	peer := &Server{ca: ca, cert: issue(ca)}
	st, err := standby.FetchStatus(ctx, addr, s.fingerprint, peer.groupClientCert())
	if err != nil || st.Term != 3 {
		t.Fatalf("同一组的面板应能拿到状态：%+v %v", st, err)
	}
	// 没开安全入口：和以前一样公开（旧版本的面板来问也能拿到）。
	if _, err := s.auth.SetEntrance(ctx, false); err != nil {
		t.Fatal(err)
	}
	if code := scan(nil, []string{"http/1.1"}); code != http.StatusOK {
		t.Fatalf("没开安全入口时应照常回答，实际 %d", code)
	}
}
