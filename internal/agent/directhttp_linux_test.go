//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 加密 HTTP 下载整条路径：节点的 Caddy 在单独的端口上开规则（只匹配 IP 和下载路径），面板那边登记、派生密钥、
// 生成命令，用真的 curl 和 openssl 跑命令下载、解密、核对 SHA-256。端口被占用时不开、说明原因。
func TestDirectHTTPThroughCaddy(t *testing.T) {
	if !nodeAccessSupported() {
		t.Skip("这个系统不支持节点访问")
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()

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
	key[0] = 5
	a.direct.keys["fp-a"] = key
	a.direct.address = "127.0.0.1" // 测试里用回环地址
	if err := a.direct.setHTTPPort(port); err != nil {
		t.Fatal(err)
	}

	p := proxy.New(filepath.Join(dataDir, "caddy"), slog.Default())
	defer p.Stop()
	proxy.SetDirectHandler(a.direct)
	// 没有开 HTTPS（测试里不申请证书）：加密 HTTP 作为备用开着。
	p.SetDirectHTTP(a.direct.wantedHTTP(p, ""))
	if err := p.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if st := a.direct.status(p); !st.GetHttpReady() || st.GetHttpPort() != uint32(port) {
		t.Fatalf("没有开好：%v", st)
	}
	// 开着以后再生成配置，端口是自己占着的，不算被占用。
	p.SetDirectHTTP(a.direct.wantedHTTP(p, ""))
	if st := a.direct.status(p); !st.GetHttpReady() || st.GetHttpError() != "" {
		t.Fatalf("重新生成配置以后：%v", st)
	}

	dir := t.TempDir()
	content := bytes.Repeat([]byte("nagi-http-"), 300000)
	file := filepath.Join(dir, "it's big.bin")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// 面板这一边：签链接、登记、派生密钥、生成命令（和 nodes.Conn.DirectHTTP 一样）。
	c := directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: file, ID: directdl.NewID(), User: "alice", Expires: time.Now().Add(time.Minute).Unix()}
	token := directdl.SignHTTP(key, c)
	s := &agentService{node: a, panel: "fp-a"}
	res, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	url := directdl.HTTPURL("127.0.0.1", port, token)
	cmd := directdl.Command{URL: url, Name: res.GetName(), Key: directdl.FileKey(key, c, res.GetEtag()), SHA256: res.GetSha256(), Resume: true}

	// 别的路径、别的 Host 是 404。
	for _, bad := range []string{"http://127.0.0.1:" + strconv.Itoa(port) + "/", "http://localhost:" + strconv.Itoa(port) + directdl.HTTPPathPrefix + token} {
		r, err := http.Get(bad)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s：%d", bad, r.StatusCode)
		}
	}
	// 经 Caddy 下载的是密文。
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || len(enc) != len(content) || bytes.Equal(enc, content) {
		t.Fatalf("下载：%d %d 字节", r.StatusCode, len(enc))
	}

	for _, tool := range []string{"sh", "curl", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s，不跑命令", tool)
		}
	}
	work := t.TempDir()
	// 先放一半密文，模拟断开的下载：命令接着续传。
	if err := os.WriteFile(filepath.Join(work, res.GetName()+"."+cmd.Key.Tag+".part"), enc[:1234567], 0o644); err != nil {
		t.Fatal(err)
	}
	sh := exec.Command("sh", "-c", cmd.String())
	sh.Dir = work
	out, err := sh.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
		t.Fatalf("命令：%v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(work, res.GetName()))
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != res.GetSha256() || !bytes.Equal(got, content) {
		t.Fatal("解密的内容不对")
	}

	// 端口被别的程序占着：不开，说明原因。
	p.Stop()
	busy, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		t.Skip("占不了端口：", err)
	}
	defer busy.Close()
	p2 := proxy.New(filepath.Join(dataDir, "caddy2"), slog.Default())
	defer p2.Stop()
	if addr, _ := a.direct.wantedHTTP(p2, ""); addr != "" {
		t.Error("端口被占用时不该开")
	}
	if st := a.direct.status(p2); st.GetHttpReady() || !strings.Contains(st.GetHttpError(), strconv.Itoa(port)) {
		t.Errorf("端口被占用的状态：%v", st)
	}
}
