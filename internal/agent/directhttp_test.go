package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func httpDLRequest(a *agent, method, path, rangeHeader string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://203.0.113.5"+path, nil)
	r.RemoteAddr = "198.51.100.8:40000"
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	w := httptest.NewRecorder()
	a.direct.ServeHTTP(w, r)
	return w
}

// 端口：存在节点上，默认 80（0 也是默认）；443 和超出范围的端口不接受。没有单独的开关，关了直连下载就不上报。
func TestDirectHTTPPort(t *testing.T) {
	a, _ := directAuditAgent(t)
	d := a.direct
	if d.httpPort() != 80 {
		t.Fatal("默认应该是 80")
	}
	for _, bad := range []int{443, -1, 65536} {
		if err := d.setHTTPPort(bad); err == nil {
			t.Errorf("端口 %d 不该接受", bad)
		}
	}
	if err := d.setHTTPPort(8080); err != nil || d.httpPort() != 8080 {
		t.Fatalf("改端口：%v %d", err, d.httpPort())
	}
	st := d.status(nil)
	if !st.GetHttpSupported() || st.GetHttpPort() != 8080 || st.GetHttpReady() {
		t.Errorf("状态：%v", st)
	}
	if err := d.setHTTPPort(0); err != nil || d.httpPort() != 80 {
		t.Fatalf("改回默认：%v %d", err, d.httpPort())
	}
	// 文件内容不对时按默认。
	if err := os.WriteFile(directHTTPFile(a.cfg.DataDir), []byte("443"), 0o600); err != nil || d.httpPort() != 80 {
		t.Fatalf("内容不对：%v %d", err, d.httpPort())
	}
	// HTTPS 能用（已经有这个 IP 的证书）时不开加密 HTTP；没有证书时作为备用开着（端口要空闲，这里用一个空闲的端口）。
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	free := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if err := d.setHTTPPort(free); err != nil {
		t.Fatal(err)
	}
	storage := t.TempDir()
	p := proxy.New(storage, slog.Default())
	// 按需监听：没有登记过链接、没有正在进行的下载时不开。
	if addr, _ := d.wantedHTTP(p, "203.0.113.5"); addr != "" {
		t.Error("没有链接时不该占着端口")
	}
	if st := d.status(p); !st.GetHttpReady() || st.GetHttpPort() != uint32(free) {
		t.Errorf("没在监听时也应上报可以用加密 HTTP：%v", st)
	}
	if err := d.startHTTP(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err) // 没有 reload（没有 Caddy）时不检查
	}
	if addr, port := d.wantedHTTP(p, "203.0.113.5"); addr != "203.0.113.5" || port != free {
		t.Errorf("登记了链接、还没有证书时应开加密 HTTP：%q %d", addr, port)
	}
	// 端口被别的程序占着：不开，记下原因，上报。
	busy, err := net.Listen("tcp", ":"+strconv.Itoa(free))
	if err != nil {
		t.Fatal(err)
	}
	if addr, _ := d.wantedHTTP(p, "203.0.113.5"); addr != "" {
		t.Error("端口被占用时不该开")
	}
	if st := d.status(p); !strings.Contains(st.GetHttpError(), strconv.Itoa(free)) {
		t.Errorf("端口被占用的原因：%v", st)
	}
	busy.Close()
	// 最后一个链接过期 httpIdle 以后、没有正在进行的下载：不再开。
	d.mu.Lock()
	d.httpUntil = time.Now().Add(-time.Second)
	d.httpActive = 1
	d.mu.Unlock()
	if addr, _ := d.wantedHTTP(p, "203.0.113.5"); addr == "" {
		t.Error("有正在进行的下载时应开着")
	}
	d.mu.Lock()
	d.httpActive = 0
	d.mu.Unlock()
	if addr, _ := d.wantedHTTP(p, "203.0.113.5"); addr != "" {
		t.Error("空闲以后不该再开")
	}
	d.mu.Lock()
	d.httpUntil = time.Now().Add(time.Minute)
	d.mu.Unlock()
	cert := filepath.Join(storage, "certificates", "acme", "203.0.113.5", "203.0.113.5.crt")
	if err := os.MkdirAll(filepath.Dir(cert), 0o700); err != nil || os.WriteFile(cert, []byte("x"), 0o600) != nil {
		t.Fatal(err)
	}
	if addr, _ := d.wantedHTTP(p, "203.0.113.5"); addr != "" {
		t.Error("HTTPS 能用时不该开加密 HTTP")
	}
	// 关了直连下载：加密 HTTP 也不开，不上报端口。
	if err := os.WriteFile(directOffFile(a.cfg.DataDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := d.status(nil); st.GetHttpPort() != 0 || st.GetHttpReady() {
		t.Errorf("关了直连下载的状态：%v", st)
	}
	if addr, _ := d.wantedHTTP(nil, ""); addr != "" {
		t.Error("关了直连下载时不该开")
	}
}

// 加密 HTTP 下载的链接核对：没开启、签名不对、过期、HTTPS 的链接、没登记过的一律 404；HTTP 的链接不能走 HTTPS 的路径。
// 只有签发它的面板能登记。
func TestDirectHTTPAuth(t *testing.T) {
	a, key := directAuditAgent(t)
	c := directdl.Claims{Node: "n1", Kind: directdl.KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.txt",
		ID: "00000000000000aa", Expires: time.Now().Add(time.Minute).Unix()}
	httpTok := directdl.SignHTTP(key, c)
	httpsTok := directdl.Sign(key, c)
	// 关了直连下载：不能登记，请求一律 404。
	off := directOffFile(a.cfg.DataDir)
	if err := os.WriteFile(off, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s0 := &agentService{node: a, panel: "fp-a"}
	if _, err := s0.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: httpTok}); err == nil {
		t.Error("关了直连下载时不该能登记")
	}
	if w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+httpTok, ""); w.Code != http.StatusNotFound {
		t.Errorf("关了直连下载：%d", w.Code)
	}
	if err := os.Remove(off); err != nil {
		t.Fatal(err)
	}
	expired := c
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	for name, path := range map[string]string{
		"没登记过":      directdl.HTTPPathPrefix + httpTok,
		"HTTPS 的链接": directdl.HTTPPathPrefix + httpsTok,
		"乱写的":       directdl.HTTPPathPrefix + "abc.def",
		"过期":        directdl.HTTPPathPrefix + directdl.SignHTTP(key, expired),
		"HTTPS 的路径": directdl.PathPrefix + httpsTok, // 明文 HTTP 上不发未加密的内容
	} {
		w := httpDLRequest(a, http.MethodGet, path, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s：%d", name, w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s：不该允许跨域", name)
		}
	}
	// HTTP 的链接拿到 HTTPS 的路径上也不行。
	if w := directRequest(a, http.MethodGet, httpTok, ""); w.Code != http.StatusForbidden {
		t.Errorf("HTTP 的链接走 HTTPS：%d", w.Code)
	}
	// 只有签发它的面板能登记；HTTPS 的链接不能登记。
	other := &agentService{node: a, panel: "fp-x"}
	if _, err := other.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: httpTok}); err == nil {
		t.Error("别的面板不该能登记")
	}
	s := &agentService{node: a, panel: "fp-a"}
	if _, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: httpsTok}); err == nil {
		t.Error("HTTPS 的链接不该能登记")
	}
	// 过期的记进本地审计，标明是 HTTP。
	got := directEvents(t, a)
	if len(got) != 1 || got[0].Result != "denied" || !strings.Contains(got[0].Detail, "transport=http") {
		t.Errorf("本地审计：%+v", got)
	}
}

// 整条路径（节点文件）：登记时算出 SHA-256，下载的是密文，用面板那边派生的密钥解得开；续传从中间取密钥流；
// 文件变了拒绝；目录打包的链接只能用一次。本地审计记下每次下载，不含密钥。
func TestDirectHTTPDownload(t *testing.T) {
	if !nodeAccessSupported() {
		t.Skip("这个系统不支持节点访问")
	}
	a, key := directAuditAgent(t)
	filehttp.ResetRanges()
	s := &agentService{node: a, panel: "fp-a"}
	dir := t.TempDir()
	file := filepath.Join(dir, "big.bin")
	content := bytes.Repeat([]byte("0123456789abcdef-"), 20000)
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	c := directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: file, ID: "00000000000000bb", User: "alice", Expires: time.Now().Add(time.Minute).Unix()}
	tok := directdl.SignHTTP(key, c)
	res, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if res.GetName() != "big.bin" || res.GetSize() != int64(len(content)) || res.GetArchive() || res.GetEtag() == "" || res.GetSha256() != hex.EncodeToString(sum[:]) {
		t.Fatalf("登记：%v", res)
	}
	ck := directdl.FileKey(key, c, res.GetEtag())

	w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok, "")
	body, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK || len(body) != len(content) || bytes.Equal(body, content) {
		t.Fatalf("下载：%d %d 字节", w.Code, len(body))
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("不该允许跨域")
	}
	plain := make([]byte, len(body))
	ck.Stream(0).XORKeyStream(plain, body)
	if !bytes.Equal(plain, content) {
		t.Fatal("解密的内容不对")
	}
	// 续传：拼起来和整个的密文一样。
	w = httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok, "bytes=100001-")
	part, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusPartialContent || !bytes.Equal(part, body[100001:]) {
		t.Fatalf("续传：%d %d 字节", w.Code, len(part))
	}
	// 换一个链接（同一个文件、同一个版本）是另一个密钥：不同链接的密钥流不同。
	c2 := c
	c2.ID = "00000000000000cc"
	res2, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: directdl.SignHTTP(key, c2)})
	if err != nil || directdl.FileKey(key, c2, res2.GetEtag()) == ck {
		t.Fatalf("不同链接的密钥应该不同：%v", err)
	}
	// 链接只认第一次下载的来源地址：别的地址 404。
	other := httptest.NewRequest(http.MethodGet, "http://203.0.113.5"+directdl.HTTPPathPrefix+tok, nil)
	other.RemoteAddr = "192.0.2.99:1234"
	ow := httptest.NewRecorder()
	a.direct.ServeHTTP(ow, other)
	if ow.Code != http.StatusNotFound {
		t.Errorf("别的来源地址：%d", ow.Code)
	}
	// HTTPS 上不认 HTTP 的链接。
	tw := httptest.NewRecorder()
	a.direct.ServeHTTP(tw, httptest.NewRequest(http.MethodGet, "https://203.0.113.5"+directdl.HTTPPathPrefix+tok, nil))
	if tw.Code != http.StatusNotFound {
		t.Errorf("HTTPS 上的 HTTP 链接：%d", tw.Code)
	}
	// 发出的内容有上限：文件大小加 linkAllowance。在上限之内照常下载，超出的那次中断，之后 404。
	tok3c := c
	tok3c.ID = "00000000000000ee"
	tok3 := directdl.SignHTTP(key, tok3c)
	if _, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: tok3}); err != nil {
		t.Fatal(err)
	}
	full := (int64(len(content)) + linkAllowance) / int64(len(content))
	for i := range full {
		if w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok3, ""); w.Code != http.StatusOK || w.Body.Len() != len(content) {
			t.Fatalf("第 %d 次：%d %d", i+1, w.Code, w.Body.Len())
		}
	}
	func() {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler {
				t.Errorf("超出上限时应中断：%v", r)
			}
		}()
		httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok3, "")
	}()
	if w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok3, "bytes=0-9"); w.Code != http.StatusNotFound {
		t.Errorf("到了上限以后：%d", w.Code)
	}
	// 文件变了：拒绝，提示重新获取。
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	if w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+tok, "bytes=5-"); w.Code != http.StatusConflict {
		t.Errorf("文件变了：%d %s", w.Code, w.Body.String())
	}

	// 目录打包：没有 SHA-256，只能下载一次。
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	dc := directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: sub, ID: "00000000000000dd", User: "alice", Expires: time.Now().Add(time.Minute).Unix()}
	dtok := directdl.SignHTTP(key, dc)
	dres, err := s.PrepareDirectHttp(context.Background(), &nagiv1.PrepareDirectHttpRequest{Token: dtok})
	if err != nil || !dres.GetArchive() || dres.GetSha256() != "" || dres.GetHashSkipped() != directdl.HashSkippedArchive || dres.GetName() != "sub.tar.gz" {
		t.Fatalf("目录登记：%v %v", dres, err)
	}
	w = httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+dtok, "")
	enc, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK {
		t.Fatalf("目录下载：%d", w.Code)
	}
	dec := make([]byte, len(enc))
	directdl.ArchiveKey(key, dc).Stream(0).XORKeyStream(dec, enc)
	gz, err := gzip.NewReader(bytes.NewReader(dec))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if strings.HasSuffix(h.Name, "x.txt") {
			found = true
		}
	}
	if !found {
		t.Error("解密后的打包里没有文件")
	}
	if w := httpDLRequest(a, http.MethodGet, directdl.HTTPPathPrefix+dtok, ""); w.Code != http.StatusNotFound {
		t.Errorf("目录打包的链接第二次：%d", w.Code)
	}

	// 本地审计：每次下载都记，标明 HTTP；不含密钥。
	events := directEvents(t, a)
	keyHex := hex.EncodeToString(ck.Key[:])
	ok := 0
	for _, e := range events {
		if strings.Contains(e.Detail+e.Error+e.Target, keyHex) {
			t.Fatal("本地审计里不能有密钥")
		}
		if !strings.Contains(e.Detail, "transport=http") {
			t.Errorf("没有标明 HTTP：%+v", e)
		}
		if e.Result == "ok" && (strings.Contains(e.Detail, "link=00000000000000bb") || strings.Contains(e.Detail, "link=00000000000000dd")) {
			ok++
		}
	}
	if ok != 3 {
		t.Errorf("应记三次成功的下载（整个文件、续传、目录）：%+v", events)
	}
}
