//go:build linux

package agent

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/directdl"
)

// 直连下载整条路径（除了向 Let's Encrypt 申请证书）：节点的 Caddy 在 443 上为 IP 开直连，证书换成 Caddy 的内部 CA；
// 客户端像浏览器连 IP 一样不发 SNI，拿签名链接探测（HEAD）、下载、续传。要能监听 443（Docker 里以 root 运行）。
func TestDirectDownloadThroughCaddy(t *testing.T) {
	if l, err := net.Listen("tcp", ":443"); err != nil {
		t.Skip("监听不了 443：", err)
	} else {
		l.Close()
	}
	proxy.DirectIssuer = map[string]any{"module": "internal"}
	defer func() { proxy.DirectIssuer = map[string]any{"module": "acme", "profile": "shortlived"} }()

	obtained := make(chan struct{}, 1)
	proxy.OnCertEvent(func(domain string, err error) {
		if domain == "127.0.0.1" && err == nil {
			select {
			case obtained <- struct{}{}:
			default:
			}
		}
	})
	defer proxy.OnCertEvent(nil)

	dataDir := t.TempDir()
	id, err := loadIdentity(filepath.Join(dataDir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := id.addPanel(trustedPanel{Address: "a:1", Fingerprint: "fp-a", NodeID: "n1", CertPEM: "cert", CAPEM: "ca"}); err != nil {
		t.Fatal(err)
	}
	a := &agent{id: id, log: slog.Default(), cfg: Config{DataDir: dataDir},
		files: &nodefiles.Service{Denied: []string{dataDir}, Managed: func() []string { return nil }}}
	a.direct = newDirectDL(a)
	key := make([]byte, 32)
	key[0] = 7
	a.direct.keys["fp-a"] = key
	// 测试里用回环地址（真用时只有公网地址才开）。
	a.direct.address = "127.0.0.1"

	p := proxy.New(filepath.Join(dataDir, "caddy"), slog.Default())
	defer p.Stop()
	proxy.SetDirectHandler(a.direct)
	p.SetDirect(a.direct.wanted(p))
	if err := p.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if p.Direct() != "127.0.0.1" {
		t.Fatalf("直连没有开：%q", p.Direct())
	}

	// 要下载的文件（节点文件）。
	dir := t.TempDir()
	content := strings.Repeat("0123456789", 10000)
	file := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if !nodeAccessSupported() || NodeAccessDisabled(dataDir) {
		t.Skip("这个系统不支持节点访问")
	}
	token := directdl.Sign(key, directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: file, Expires: time.Now().Add(time.Minute).Unix()})
	url := "https://127.0.0.1" + directdl.PathPrefix + token
	// 不发 SNI（连 IP 时浏览器就是这样），内部 CA 的证书不校验。
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// 证书由 Caddy 在后台签发，刚加载完可能还没好。
	var res *http.Response
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodHead, url, nil)
		res, err = client.Do(req)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("探测：%d %v", res.StatusCode, res.Header)
	}
	select {
	case <-obtained:
	case <-time.After(time.Until(deadline)):
		t.Fatal("签发成功但节点没有收到证书事件")
	}

	// 整个文件。
	res, err = client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != content || !strings.Contains(res.Header.Get("Content-Disposition"), "big.txt") {
		t.Fatalf("下载：%d，%d 字节，%v", res.StatusCode, len(body), res.Header)
	}
	etag := res.Header.Get("ETag")

	// 续传：从中间开始，带 If-Range。
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=50000-")
	req.Header.Set("If-Range", etag)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || string(body) != content[50000:] ||
		res.Header.Get("Content-Range") != fmt.Sprintf("bytes 50000-%d/%d", len(content)-1, len(content)) {
		t.Fatalf("续传：%d，%d 字节，%v", res.StatusCode, len(body), res.Header)
	}

	// 改过的链接、别的路径。
	for _, bad := range []string{url[:len(url)-2] + "xx", "https://127.0.0.1/"} {
		res, err := client.Get(bad)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusNotFound {
			t.Errorf("%s：%d", bad, res.StatusCode)
		}
	}

	// 状态：拿到了这个 IP 的证书（内部 CA 也存在 certificates/ 下）。
	st := a.direct.status(p)
	if !st.GetReady() || st.GetAddress() != "127.0.0.1" {
		t.Errorf("状态：%v", st)
	}
}
