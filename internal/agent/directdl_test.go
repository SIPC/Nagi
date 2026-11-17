package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 直连下载的链接核对：信任列表里的面板给的密钥签的、写的是本节点、没过期才放行；跨域探测（HEAD）不读文件；
// 关了直连、不是这个路径、不是 GET/HEAD 的一律 404。
func TestDirectDownloadAuth(t *testing.T) {
	dataDir := t.TempDir()
	id, err := loadIdentity(filepath.Join(dataDir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := id.addPanel(trustedPanel{Address: "a:1", Fingerprint: "fp-a", NodeID: "n1", CertPEM: "cert", CAPEM: "ca"}); err != nil {
		t.Fatal(err)
	}
	a := &agent{id: id, log: slog.Default(), cfg: Config{DataDir: dataDir}}
	a.direct = newDirectDL(a)
	key, stranger := make([]byte, 32), make([]byte, 32)
	key[0], stranger[0] = 1, 2
	s := &agentService{node: a, panel: "fp-a"}
	if _, err := s.SetDirectDownload(context.Background(), &nagiv1.SetDirectDownloadRequest{Key: key, Address: "203.0.113.5"}); err != nil {
		t.Fatal(err)
	}
	// 不信任的面板（不在信任列表里）给的密钥。
	other := &agentService{node: a, panel: "fp-x"}
	if _, err := other.SetDirectDownload(context.Background(), &nagiv1.SetDirectDownloadRequest{Key: stranger, Address: "203.0.113.5"}); err != nil {
		t.Fatal(err)
	}
	// 内网地址不用来直连。
	if a.direct.address != "203.0.113.5" {
		t.Fatalf("地址：%q", a.direct.address)
	}
	if _, err := s.SetDirectDownload(context.Background(), &nagiv1.SetDirectDownloadRequest{Key: key, Address: "10.0.0.5"}); err != nil || a.direct.address != "" {
		t.Fatalf("内网地址应该不用：%q %v", a.direct.address, err)
	}
	_, _ = s.SetDirectDownload(context.Background(), &nagiv1.SetDirectDownloadRequest{Key: key, Address: "203.0.113.5"})

	link := func(k []byte, node string, ttl time.Duration) string {
		return directdl.Sign(k, directdl.Claims{Node: node, Kind: directdl.KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.txt",
			Expires: time.Now().Add(ttl).Unix()})
	}
	do := func(method, token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.direct.ServeHTTP(w, httptest.NewRequest(method, "https://203.0.113.5"+directdl.PathPrefix+token, nil))
		return w
	}
	cases := []struct {
		name   string
		method string
		token  string
		want   int
	}{
		{"有效的链接：探测", http.MethodHead, link(key, "n1", time.Minute), http.StatusNoContent},
		{"不信任的面板的密钥", http.MethodHead, link(stranger, "n1", time.Minute), http.StatusForbidden},
		{"写的不是本节点", http.MethodHead, link(key, "n2", time.Minute), http.StatusForbidden},
		{"过期", http.MethodHead, link(key, "n1", -time.Minute), http.StatusForbidden},
		{"乱写的", http.MethodGet, "abc.def", http.StatusForbidden},
		{"不是 GET、HEAD", http.MethodPost, link(key, "n1", time.Minute), http.StatusNotFound},
	}
	for _, c := range cases {
		w := do(c.method, c.token)
		if w.Code != c.want {
			t.Errorf("%s：%d，想要 %d（%s）", c.name, w.Code, c.want, w.Body.String())
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s：没有 CORS 头", c.name)
		}
	}
	// 别的路径。
	w := httptest.NewRecorder()
	a.direct.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://203.0.113.5/", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("别的路径：%d", w.Code)
	}
	// 跨域预检。
	w = httptest.NewRecorder()
	a.direct.ServeHTTP(w, httptest.NewRequest(http.MethodOptions, "https://203.0.113.5"+directdl.PathPrefix+"x", nil))
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("预检：%d %v", w.Code, w.Header())
	}
	// 节点文件要节点访问开着。
	nodeLink := directdl.Sign(key, directdl.Claims{Node: "n1", Kind: directdl.KindNode, Path: "/etc/hostname", Expires: time.Now().Add(time.Minute).Unix()})
	if err := SetNodeAccess(dataDir, false); err != nil {
		t.Fatal(err)
	}
	if w := do(http.MethodHead, nodeLink); w.Code != http.StatusForbidden {
		t.Errorf("节点访问关着时节点文件应该拒绝：%d", w.Code)
	}
	// 关了直连：一律 404。
	if err := os.WriteFile(directOffFile(dataDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := do(http.MethodHead, link(key, "n1", time.Minute)); w.Code != http.StatusNotFound {
		t.Errorf("关了直连：%d", w.Code)
	}
	if st := a.direct.status(nil); !st.GetOff() || st.GetReady() {
		t.Errorf("关了直连的状态：%v", st)
	}
}

// 证书事件：直连的 IP 只记状态（不交给按域名告警的逻辑），别的域名照旧。
func TestDirectCertEvent(t *testing.T) {
	d := newDirectDL(&agent{cfg: Config{DataDir: t.TempDir()}})
	d.address = "203.0.113.5"
	if d.certEvent("example.com", nil) {
		t.Error("别的域名不该当成直连的 IP")
	}
	if !d.certEvent("203.0.113.5", os.ErrDeadlineExceeded) || d.certErr == "" {
		t.Error("直连的 IP 申请失败应该记下原因")
	}
	if !d.certEvent("203.0.113.5", nil) || d.certErr != "" {
		t.Error("成功以后应该清掉原因")
	}
}

// 上报里的直连状态每次现算：启动时缓存的 a.info 算出来时 a.direct 还没有创建；状态也会随开关变。
// 面板要看到这个字段才会告诉节点密钥，少了它直连下载永远不会开。
func TestReportDirectStatus(t *testing.T) {
	dataDir := t.TempDir()
	a := &agent{cfg: Config{DataDir: dataDir}, info: &nagiv1.NodeInfo{}, replicas: newReplicas(dataDir)}
	a.direct = newDirectDL(a)
	st := a.liveInfo().GetDirectDownload()
	if !st.GetSupported() || st.GetOff() {
		t.Fatalf("开着：%v", st)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directOffFile(dataDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := a.liveInfo().GetDirectDownload(); !st.GetOff() {
		t.Errorf("关了：%v", st)
	}
}
