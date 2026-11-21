package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/auditlog"
	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
)

// directAuditAgent 是一个信任 fp-a（本节点在它上面是 n1）、有本地审计的节点，返回它和 fp-a 给的密钥。
func directAuditAgent(t *testing.T) (*agent, []byte) {
	t.Helper()
	dataDir := t.TempDir()
	id, err := loadIdentity(filepath.Join(dataDir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := id.addPanel(trustedPanel{Address: "a:1", Fingerprint: "fp-a", NodeID: "n1", CertPEM: "cert", CAPEM: "ca"}); err != nil {
		t.Fatal(err)
	}
	l, err := auditlog.Open(auditlog.Path(filepath.Join(dataDir, "logs")), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	a := &agent{id: id, log: slog.Default(), cfg: Config{DataDir: dataDir}, audit: l,
		files: &nodefiles.Service{Denied: []string{dataDir}, Managed: func() []string { return nil }}}
	a.direct = newDirectDL(a)
	key := make([]byte, 32)
	key[0] = 9
	a.direct.keys["fp-a"] = key
	a.direct.address = "203.0.113.5"
	return a, key
}

func directEvents(t *testing.T, a *agent) []auditlog.Event {
	t.Helper()
	var out []auditlog.Event
	for after := int64(0); ; {
		lines, _, last := a.audit.Read(after, 500)
		if len(lines) == 0 {
			return out
		}
		after = last
		for _, b := range lines {
			var e auditlog.Event
			if err := json.Unmarshal(b, &e); err != nil {
				t.Fatal(err)
			}
			if e.Action == "direct_download" {
				out = append(out, e)
			}
		}
	}
}

func directRequest(a *agent, method, token, rangeHeader string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://203.0.113.5"+directdl.PathPrefix+token, nil)
	r.RemoteAddr = "198.51.100.7:51234"
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	w := httptest.NewRecorder()
	a.direct.ServeHTTP(w, r)
	return w
}

// L3：直连下载记进节点的本地审计（链接编号、签发的成员、来源地址、范围、对象、签发的面板），随节点的审计外送。
// 签名对不上的请求和探测（HEAD）不记；过期的链接记为 denied。
func TestDirectDownloadAudit(t *testing.T) {
	a, key := directAuditAgent(t)
	link := func(c directdl.Claims, ttl time.Duration) string {
		c.Node, c.Expires = "n1", time.Now().Add(ttl).Unix()
		return directdl.Sign(key, c)
	}
	vol := directdl.Claims{Kind: directdl.KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.txt", ID: "0123456789abcdef", User: "bob"}

	directRequest(a, http.MethodGet, "abc.def", "")
	directRequest(a, http.MethodHead, link(vol, time.Minute), "")
	if got := directEvents(t, a); len(got) != 0 {
		t.Fatalf("乱写的链接和探测不该记：%+v", got)
	}

	if w := directRequest(a, http.MethodGet, link(vol, -time.Minute), ""); w.Code != http.StatusForbidden {
		t.Fatalf("过期：%d", w.Code)
	}
	got := directEvents(t, a)
	if len(got) != 1 {
		t.Fatalf("过期的链接应记一条：%+v", got)
	}
	e := got[0]
	if e.Result != "denied" || e.Actor != "bob" || e.IP != "198.51.100.7" || e.Panel != "fp-a" ||
		e.Target != "p/web/data:a.txt" || !strings.Contains(e.Detail, "link=0123456789abcdef") || e.Error == "" {
		t.Fatalf("过期的记录：%+v", e)
	}

	// 下载节点上的文件：整个文件每次都记，同一个链接同一个地址的分段请求 10 分钟内只记第一次。
	if !nodeAccessSupported() {
		// 这个系统不支持节点访问：节点文件的链接被拒绝，也记下来。
		nodeLink := link(directdl.Claims{Kind: directdl.KindNode, Path: "/etc/hostname", User: "alice"}, time.Minute)
		directRequest(a, http.MethodGet, nodeLink, "")
		got := directEvents(t, a)
		if len(got) != 2 || got[1].Result != "denied" || got[1].Target != "/etc/hostname" || !strings.Contains(got[1].Detail, "kind=node") {
			t.Fatalf("节点文件被拒绝的记录：%+v", got)
		}
		return
	}
	filehttp.ResetRanges()
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	content := strings.Repeat("0123456789", 1000)
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	nodeLink := link(directdl.Claims{Kind: directdl.KindNode, Path: file, User: "alice"}, time.Minute)
	for i := range 2 {
		w := directRequest(a, http.MethodGet, nodeLink, "")
		if b, _ := io.ReadAll(w.Body); w.Code != http.StatusOK || string(b) != content {
			t.Fatalf("第 %d 次下载：%d %d 字节", i+1, w.Code, len(b))
		}
	}
	for _, rg := range []string{"bytes=0-99", "bytes=100-199", "bytes=200-"} {
		if w := directRequest(a, http.MethodGet, nodeLink, rg); w.Code != http.StatusPartialContent {
			t.Fatalf("分段 %s：%d", rg, w.Code)
		}
	}
	got = directEvents(t, a)[1:]
	if len(got) != 3 {
		t.Fatalf("应记两次整个文件和一次分段：%+v", got)
	}
	for _, e := range got {
		if e.Result != "ok" || e.Actor != "alice" || e.Target != file || !strings.Contains(e.Detail, "link=sha256:") {
			t.Errorf("下载的记录：%+v", e)
		}
	}
	if !strings.Contains(got[2].Detail, "range=bytes=0-99") {
		t.Errorf("分段的记录要写范围：%+v", got[2])
	}
}

// 本地审计里直连下载的记录有限额：拿着有效链接反复请求刷不满本地审计，超出的只计数，写进下一段的第一条。
func TestDirectDownloadAuditLimit(t *testing.T) {
	a, key := directAuditAgent(t)
	c := directdl.Claims{Node: "n1", Kind: directdl.KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.txt",
		Expires: time.Now().Add(-time.Minute).Unix()}
	expired := directdl.Sign(key, c)
	for range directAuditLimit + 7 {
		directRequest(a, http.MethodGet, expired, "")
	}
	if n := len(directEvents(t, a)); n != directAuditLimit {
		t.Fatalf("应只记 %d 条，实际 %d", directAuditLimit, n)
	}
	// 下一段时间：第一条写明上一段没记下的次数。
	a.direct.mu.Lock()
	a.direct.auditStart = time.Now().Add(-directAuditWindow)
	a.direct.mu.Unlock()
	directRequest(a, http.MethodGet, expired, "")
	got := directEvents(t, a)
	if last := got[len(got)-1]; !strings.Contains(last.Detail, "dropped=7") {
		t.Fatalf("应写明没记下的次数：%+v", last)
	}
}
