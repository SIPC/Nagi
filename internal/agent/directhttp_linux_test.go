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
	"net/http/httptest"
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
	a.proxy = p
	// 和 agent.applyProxy 一样重新生成配置（没有 HTTPS：测试里不申请证书）。
	a.direct.reload = func(context.Context) {
		p.SetDirectHTTP(a.direct.wantedHTTP(p, ""))
		if err := p.Apply(nil); err != nil {
			t.Error(err)
		}
	}
	listening := func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	// 按需监听：还没有链接时不占端口，但上报可以用加密 HTTP。
	a.direct.reload(context.Background())
	if listening() {
		t.Fatal("没有链接时不该监听")
	}
	if st := a.direct.status(p); !st.GetHttpReady() || st.GetHttpPort() != uint32(port) {
		t.Fatalf("状态：%v", st)
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
	// 登记以后才开始监听；再生成配置时端口是自己占着的，不算被占用。
	if !listening() {
		t.Fatal("登记了链接应开始监听")
	}
	a.direct.reload(context.Background())
	if st := a.direct.status(p); !st.GetHttpReady() || st.GetHttpError() != "" || !listening() {
		t.Fatalf("重新生成配置以后：%v", st)
	}
	url := directdl.HTTPURL("127.0.0.1", port, token)
	cmd := directdl.Command{URL: url, Name: res.GetName(), Key: directdl.FileKey(key, c, res.GetEtag()), SHA256: res.GetSha256(), Resume: true, Size: res.GetSize()}

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
	partial := filepath.Join(work, res.GetName()+"."+cmd.Key.Tag+".1.part0")
	if err := os.WriteFile(partial, enc[:1234567], 0o644); err != nil {
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
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("续传片段未被消费清理: %v", err)
	}

	// 端到端（真的 Caddy、curl）：别的来源地址拿同一个链接是 404（127.0.0.2 也在回环上）。
	codeOf := func(args ...string) string {
		out, _ := exec.Command("curl", append([]string{"-s", "-o", "/dev/null", "-w", "%{http_code}"}, args...)...).Output()
		return string(out)
	}
	if got := codeOf("--interface", "127.0.0.2", url); got != "404" {
		t.Errorf("别的来源地址：%s", got)
	}
	// 下载中途文件被改写：curl 慢慢下载时改写文件，连接被中断（curl 失败），之后同一个链接 409。
	old := recheckEvery
	recheckEvery = 1 << 20
	defer func() { recheckEvery = old }()
	db := filepath.Join(dir, "db.bin")
	if err := os.WriteFile(db, bytes.Repeat([]byte("0123456789abcdef"), 2<<20), 0o644); err != nil { // 32 MiB
		t.Fatal(err)
	}
	cdb := directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: db, ID: directdl.NewID(), User: "alice", Expires: time.Now().Add(time.Minute).Unix()}
	tdb := directdl.SignHTTP(key, cdb)
	if _, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: tdb}); err != nil {
		t.Fatal(err)
	}
	udb := directdl.HTTPURL("127.0.0.1", port, tdb)
	slow := exec.Command("curl", "-sf", "--limit-rate", "4M", "-o", filepath.Join(work, "db.part"), udb)
	if err := slow.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := os.WriteFile(db, bytes.Repeat([]byte("fedcba9876543210"), 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(db, later, later)
	if err := slow.Wait(); err == nil {
		t.Error("文件中途被改写时 curl 应失败")
	}
	if fi, err := os.Stat(filepath.Join(work, "db.part")); err == nil && fi.Size() >= 32<<20 {
		t.Errorf("不该收到完整的内容：%d", fi.Size())
	}
	if got := codeOf(udb); got != "409" {
		t.Errorf("改写以后同一个链接：%s", got)
	}

	// 空闲以后（最后一个链接过期 httpIdle 以后、没有正在进行的下载）关掉端口。
	a.direct.mu.Lock()
	a.direct.httpUntil = time.Now().Add(-time.Second)
	a.direct.mu.Unlock()
	a.direct.reload(context.Background())
	if listening() {
		t.Error("空闲以后应关掉端口")
	}

	// 端口被别的程序占着：登记时报错，说明原因，上报。
	busy, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		t.Skip("占不了端口：", err)
	}
	defer busy.Close()
	c2 := c
	c2.ID = directdl.NewID()
	_, err = s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: directdl.SignHTTP(key, c2)})
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("端口被占用时登记应报错：%v", err)
	}
	if st := a.direct.status(p); !strings.Contains(st.GetHttpError(), strconv.Itoa(port)) {
		t.Errorf("端口被占用的状态：%v", st)
	}
}

// 下载中途文件被改写：中断连接，链接作废（之后 409），这个版本一段时间内也不再登记。
func TestDirectHTTPFileChangedMidStream(t *testing.T) {
	if !nodeAccessSupported() {
		t.Skip("这个系统不支持节点访问")
	}
	old := recheckEvery
	recheckEvery = 1 << 20
	defer func() { recheckEvery = old }()
	a, key := directAuditAgent(t)
	srv := httptest.NewServer(a.direct)
	defer srv.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "db.bin")
	content := bytes.Repeat([]byte("0123456789abcdef"), 4<<20) // 64 MiB
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	c := directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: file, ID: directdl.NewID(), User: "alice", Expires: time.Now().Add(time.Minute).Unix()}
	tok := directdl.SignHTTP(key, c)
	s := &agentService{node: a, panel: "fp-a"}
	res, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.Get(srv.URL + directdl.HTTPPathPrefix + tok)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 2<<20)
	if _, err := io.ReadFull(r.Body, head); err != nil {
		t.Fatal(err)
	}
	// 改写文件（大小不变，修改时间变了）。
	changed := bytes.Repeat([]byte("fedcba9876543210"), 4<<20)
	if err := os.WriteFile(file, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err == nil || int64(len(head)+len(rest)) >= res.GetSize() {
		t.Fatalf("应中断：读了 %d 字节，%v", len(head)+len(rest), err)
	}
	// 链接作废：之后（包括续传）一律 409。
	r2, err := http.Get(srv.URL + directdl.HTTPPathPrefix + tok)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("作废以后：%d", r2.StatusCode)
	}
	// 新的版本可以重新获取命令。
	c3 := c
	c3.ID = directdl.NewID()
	if _, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: directdl.SignHTTP(key, c3)}); err != nil {
		t.Errorf("新的版本应能登记：%v", err)
	}
	// 本地审计记下中断。
	found := false
	for _, e := range directEvents(t, a) {
		if e.Result == "failed" && strings.Contains(e.Error, "文件已变化") {
			found = true
		}
	}
	if !found {
		t.Error("本地审计里应记下文件中途变了")
	}
}
