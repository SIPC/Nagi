package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/standby"
	"github.com/SIPC/nagi/internal/server/store"
)

const (
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	idmUA    = "curl/8.5.0"
	linkIP   = "203.0.113.9"
)

type linkHarness struct {
	t     *testing.T
	a     *API
	exec  func(q string, args ...any)
	mux   *http.ServeMux
	data  []byte
	opens []string
}

func newLinkHarness(t *testing.T) *linkHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	h := &linkHarness{t: t, data: []byte("0123456789abcdefghijklmnopqrstuvwxyz")}
	h.exec = func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, role, all_projects, all_nodes) VALUES (1, 'ad', 'x', 0, 'admin', 1, 1)`,
		`INSERT INTO users (id, username, password_hash, created_at, role, role_id, all_projects, all_nodes) VALUES (2, 'op', 'x', 0, 'member', 2, 0, 0)`,
		`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x'')`,
		`INSERT INTO user_projects (user_id, project_id) VALUES (2, 'p1')`,
		`INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n1', 'a', 0, x'01')`,
	} {
		h.exec(q)
	}
	q := store.New(d)
	as := auth.New(q, nil)
	as.SetAudit(log)
	h.a = &API{Q: q, Auth: as, Audit: log, Log: slog.Default(), Panel: &PanelInfo{Q: q, DataDir: dir}}
	h.mux = http.NewServeMux()
	h.mux.Handle("GET /api/download/{token}", h.a.DownloadLinkHandler())
	h.mux.Handle("GET /api/volume-file", h.a.VolumeFileHandler())

	// 节点换成假的：按 Range 发内容（见 filedownload_test.go 的 fakeNode）。
	oldV, oldN := volumeSourceFor, nodeSourceFor
	t.Cleanup(func() { volumeSourceFor, nodeSourceFor = oldV, oldN })
	volumeSourceFor = func(_ *API, _ context.Context, ref *nagiv1.VolumeFileRef) (fileSource, error) {
		h.opens = append(h.opens, ref.GetProjectId()+"/"+ref.GetContainer()+"/"+ref.GetVolume()+"/"+ref.GetPath())
		return fileSource{open: fakeNode(h.data, true), close: func() {}}, nil
	}
	nodeSourceFor = func(_ *API, _ context.Context, nodeID, path string) (fileSource, error) {
		h.opens = append(h.opens, nodeID+":"+path)
		return fileSource{open: fakeNode(h.data, true), close: func() {}}, nil
	}
	filehttp.ResetRanges()
	return h
}

// login 建一个浏览器（Chrome）的会话，返回 cookie 的值和会话的用户。
func (h *linkHarness) login(userID int64) (string, auth.User) {
	h.t.Helper()
	ctx := auth.WithMeta(context.Background(), auth.Meta{UserAgent: chromeUA, IP: linkIP})
	c, err := h.a.Auth.NewSession(ctx, userID, linkIP)
	if err != nil {
		h.t.Fatal(err)
	}
	u, err := h.a.Auth.SessionUser(context.Background(), c.Value)
	if err != nil {
		h.t.Fatal(err)
	}
	return c.Value, u
}

// issue 按 u 签一个链接（和 /api/volume-file?check=1&sign=1 确认后一样）。
func (h *linkHarness) issue(u auth.User, c downloadClaims) string {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/volume-file", nil)
	req.RemoteAddr = linkIP + ":5000"
	ctx := auth.WithRequest(auth.WithUser(context.Background(), u), req)
	rec := httptest.NewRecorder()
	recorded := 0
	issueDownloadLink(rec, req, ctx, u, c, func(extra map[string]any) {
		recorded++
		if extra["download_link"] == "" {
			h.t.Error("活动记录要带链接编号")
		}
	})
	if rec.Code != 200 || recorded != 1 {
		h.t.Fatalf("签发：%d %s，记录 %d 次", rec.Code, rec.Body, recorded)
	}
	var body struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.HasPrefix(body.URL, downloadLinkPath) {
		h.t.Fatalf("链接：%q", body.URL)
	}
	return body.URL
}

// get 用下载工具的身份（别的 UA）请求链接，带着浏览器的 cookie。
func (h *linkHarness) get(url string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = linkIP + ":6000"
	req.Header.Set("User-Agent", idmUA)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

var volumeClaims = downloadClaims{Kind: linkKindVolume, Project: "p1", Container: "web", Volume: "data", Path: "a.bin"}

func TestDownloadLinkServes(t *testing.T) {
	h := newLinkHarness(t)
	token, u := h.login(2)
	url := h.issue(u, volumeClaims)
	cookie := auth.CookieName + "=" + token

	// 有效期内可以多次使用（多线程、续传），带着 cookie、换了 UA 也不影响会话。
	for range 3 {
		rec := h.get(url, "Cookie", cookie)
		if rec.Code != 200 || rec.Body.String() != string(h.data) {
			t.Fatalf("下载：%d %q", rec.Code, rec.Body.String())
		}
		hd := rec.Header()
		if hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("Cache-Control") != "private, no-store" ||
			!strings.HasPrefix(hd.Get("Content-Disposition"), "attachment") {
			t.Errorf("响应头：%v", hd)
		}
	}
	rec := h.get(url, "Range", "bytes=10-15")
	if rec.Code != 206 || rec.Body.String() != "abcdef" || rec.Header().Get("Content-Range") != "bytes 10-15/36" {
		t.Errorf("Range：%d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	if h.opens[0] != "p1/web/data/a.bin" {
		t.Errorf("下载的文件：%q", h.opens)
	}

	// 会话还在：浏览器照常能用。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = linkIP + ":7000"
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Cookie", cookie)
	if _, err := h.a.Auth.RequestUser(req); err != nil {
		t.Fatalf("用过签名链接后会话失效了：%v", err)
	}
	// 对照：同样的请求直接带 cookie 访问旧的地址，会被当成换了设备、退出登录（所以界面改用签名链接）。
	token2, _ := h.login(2)
	rec = h.get("/api/volume-file?project=p1&container=web&volume=data&path=a.bin", "Cookie", auth.CookieName+"="+token2)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧地址换了 UA：%d", rec.Code)
	}
	if _, err := h.a.Auth.SessionUser(context.Background(), token2); err == nil {
		t.Error("对照组的会话应该已经退出")
	}
}

func TestDownloadLinkInvalid(t *testing.T) {
	h := newLinkHarness(t)
	_, u := h.login(2)
	url := h.issue(u, volumeClaims)
	token := strings.TrimPrefix(url, downloadLinkPath)
	flip := []byte(token)
	if flip[10] == 'A' {
		flip[10] = 'B'
	} else {
		flip[10] = 'A'
	}
	seal := func(c downloadClaims) string {
		s, err := sealDownload(c)
		if err != nil {
			t.Fatal(err)
		}
		return downloadLinkPath + s
	}
	good := volumeClaims
	good.User, good.Session, good.ID, good.Expires = u.ID, u.SessionID, "id1", time.Now().Add(time.Minute).Unix()
	expired, wrongKind, otherUser := good, good, good
	expired.Expires = time.Now().Add(-time.Second).Unix()
	wrongKind.Kind = "backup"
	otherUser.User = 1
	for name, c := range map[string]struct {
		url  string
		code int
	}{
		"改过的":     {downloadLinkPath + string(flip), 404},
		"截短的":     {url[:len(url)-4], 404},
		"不是链接":    {downloadLinkPath + "abc.def", 404},
		"过期的":     {seal(expired), 404},
		"种类不对":    {seal(wrongKind), 404},
		"会话不是这个人": {seal(otherUser), 403},
		"自己签的有效":  {seal(good), 200},
	} {
		if rec := h.get(c.url); rec.Code != c.code {
			t.Errorf("%s：%d，想要 %d", name, rec.Code, c.code)
		} else if c.code != 200 && strings.Contains(rec.Body.String(), "p1") {
			t.Errorf("%s：不应说明原因：%s", name, rec.Body)
		}
	}
	if len(h.opens) != 1 {
		t.Errorf("无效的链接不应打开文件：%q", h.opens)
	}
}

// 每次使用都重查：会话退出、用户删除、降为只读、移出项目范围后链接立即不能用。
func TestDownloadLinkRecheck(t *testing.T) {
	cases := map[string]func(h *linkHarness, token string){
		"退出登录":   func(h *linkHarness, token string) { h.a.Auth.Logout(context.Background(), token) },
		"删除用户":   func(h *linkHarness, _ string) { h.exec(`DELETE FROM users WHERE id = 2`) },
		"降为只读":   func(h *linkHarness, _ string) { h.exec(`UPDATE users SET role_id = 3 WHERE id = 2`) },
		"移出项目范围": func(h *linkHarness, _ string) { h.exec(`DELETE FROM user_projects`) },
		"要求重新验证": func(h *linkHarness, _ string) { h.exec(`UPDATE sessions SET reverify = 'tls_changed'`) },
	}
	for name, change := range cases {
		h := newLinkHarness(t)
		token, u := h.login(2)
		url := h.issue(u, volumeClaims)
		if rec := h.get(url); rec.Code != 200 {
			t.Fatalf("%s：改之前：%d", name, rec.Code)
		}
		change(h, token)
		if rec := h.get(url); rec.Code != http.StatusForbidden {
			t.Errorf("%s：%d，应为 403", name, rec.Code)
		}
	}
}

func TestDownloadLinkNode(t *testing.T) {
	h := newLinkHarness(t)
	_, admin := h.login(1)
	url := h.issue(admin, downloadClaims{Kind: linkKindNode, Node: "n1", Path: "/etc/hosts"})
	if rec := h.get(url); rec.Code != 200 || h.opens[0] != "n1:/etc/hosts" {
		t.Fatalf("节点文件：%d %q", rec.Code, h.opens)
	}
	// 管理员降为操作员后不能再用。
	h.exec(`UPDATE users SET role = 'member', role_id = 2 WHERE id = 1`)
	if rec := h.get(url); rec.Code != http.StatusForbidden {
		t.Errorf("降权后：%d", rec.Code)
	}
	// 操作员的会话签的节点文件链接（不会签发，这里直接伪造内容）也不能用。
	_, op := h.login(2)
	url = h.issue(op, downloadClaims{Kind: linkKindNode, Node: "n1", Path: "/etc/hosts"})
	if rec := h.get(url); rec.Code != http.StatusForbidden {
		t.Errorf("操作员：%d", rec.Code)
	}
	// 备用面板上不能下载节点文件，卷文件照常。
	h.exec(`UPDATE users SET role = 'admin', role_id = 1 WHERE id = 1`)
	url = h.issue(admin, downloadClaims{Kind: linkKindNode, Node: "n1", Path: "/etc/hosts"})
	h.a.Panel.Standby = &standby.Client{Info: &standby.Info{}}
	if rec := h.get(url); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("备用面板：%d", rec.Code)
	}
	if rec := h.get(h.issue(admin, volumeClaims)); rec.Code != 200 {
		t.Errorf("备用面板的卷文件：%d", rec.Code)
	}
}

// 经旧地址确认并签发：check=1&sign=1 照常按 cookie 检查，记一次下载（带链接编号），返回链接；只带 check=1 时照旧回 204、不记录。
// 链接第一次使用后绑定这个地址：别的地址 403，记一条安全事件（每个地址一次），不记下载。
func TestDownloadLinkIssueViaHandler(t *testing.T) {
	h := newLinkHarness(t)
	token, _ := h.login(2)
	browser := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/volume-file?project=p1&container=web&volume=data&path=a.bin"+query, nil)
		req.RemoteAddr = linkIP + ":5000"
		req.Header.Set("User-Agent", chromeUA)
		req.Header.Set("Cookie", auth.CookieName+"="+token)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := browser("&check=1"); rec.Code != http.StatusNoContent {
		t.Fatalf("check=1：%d %s", rec.Code, rec.Body)
	}
	rec := browser("&check=1&sign=1")
	var body struct{ URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || !strings.HasPrefix(body.URL, downloadLinkPath) || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("sign=1：%d %s", rec.Code, rec.Body)
	}
	other := func() int {
		req := httptest.NewRequest(http.MethodGet, body.URL, nil)
		req.RemoteAddr = "198.51.100.7:1000"
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := h.get(body.URL); got.Code != 200 {
		t.Fatalf("下载：%d", got.Code)
	}
	opened := len(h.opens)
	for range 2 {
		if code := other(); code != http.StatusForbidden {
			t.Errorf("别的地址：%d，应为 403", code)
		}
	}
	if len(h.opens) != opened {
		t.Error("别的地址不应打开文件")
	}
	// 绑定的地址照常能用（续传、多线程）。
	if got := h.get(body.URL, "Range", "bytes=0-3"); got.Code != 206 {
		t.Errorf("绑定的地址续传：%d", got.Code)
	}
	events, err := h.a.Audit.List(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var issued, otherIP, denied int
	for _, e := range events {
		if strings.Contains(e.Params, body.URL[len(downloadLinkPath):]) {
			t.Error("记录里不能有链接")
		}
		switch {
		case e.Action == "download_link_other_ip":
			denied++
			if e.Category != audit.Security || e.Result != audit.ResultDenied || e.IP != "198.51.100.7" || e.ActorName != "op" ||
				!strings.Contains(e.Params, `"bound_ip":"`+linkIP+`"`) || !strings.Contains(e.Params, "download_link") {
				t.Errorf("安全事件：%+v", e)
			}
		case e.Action != "DownloadVolumeFile":
		case strings.Contains(e.Params, `"other_ip":true`):
			otherIP++
		case strings.Contains(e.Params, "download_link"):
			issued++
		}
	}
	if issued != 1 || otherIP != 0 || denied != 1 {
		t.Errorf("签发记 %d 次、别的地址下载 %d 次、拒绝 %d 次，应为 1、0、1", issued, otherIP, denied)
	}
}

// 第一次使用的地址不是签发的地址（下载工具走代理、浏览器换到 IPv6）：照常下载，绑定到这个地址，记一条 other_ip；
// 之后签发的地址也不能用了（按确切的地址绑定，要重新下载）。
func TestDownloadLinkBindsFirstUse(t *testing.T) {
	h := newLinkHarness(t)
	_, u := h.login(2)
	url := h.issue(u, volumeClaims)
	const v6 = "2001:db8::9"
	from := func(addr string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.RemoteAddr = addr
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		return rec
	}
	for i := range 3 {
		rec := from("["+v6+"]:"+strconv.Itoa(7000+i), "Range", "bytes="+strconv.Itoa(i*10)+"-"+strconv.Itoa(i*10+9))
		if rec.Code != 206 || rec.Body.String() != string(h.data[i*10:i*10+10]) {
			t.Fatalf("第 %d 段：%d %q", i, rec.Code, rec.Body.String())
		}
	}
	if rec := from(linkIP + ":5000"); rec.Code != http.StatusForbidden {
		t.Errorf("签发的地址在绑定之后：%d，应为 403", rec.Code)
	}
	events, err := h.a.Audit.List(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var otherIP, denied int
	for _, e := range events {
		switch {
		case e.Action == "download_link_other_ip":
			denied++
			if e.IP != linkIP || !strings.Contains(e.Params, `"bound_ip":"`+v6+`"`) {
				t.Errorf("安全事件：%+v", e)
			}
		case e.Action == "DownloadVolumeFile" && strings.Contains(e.Params, `"other_ip":true`):
			otherIP++
			if e.IP != v6 || !strings.Contains(e.Params, linkIP) {
				t.Errorf("别的地址：%+v", e)
			}
		}
	}
	if otherIP != 1 || denied != 1 {
		t.Errorf("other_ip %d 次、拒绝 %d 次，应各 1 次", otherIP, denied)
	}
}

// 每个被拒绝的地址只报一次，记下的地址有上限；会话不再允许时不绑定，失败的请求不会占住链接。
func TestDownloadLinkBindAfterAuth(t *testing.T) {
	if b, r := bindLink("bind-test", "192.0.2.1"); b != "192.0.2.1" || r {
		t.Fatalf("第一次：%q %v", b, r)
	}
	if b, r := bindLink("bind-test", "192.0.2.2"); b != "192.0.2.1" || !r {
		t.Fatalf("别的地址：%q %v", b, r)
	}
	if _, r := bindLink("bind-test", "192.0.2.2"); r {
		t.Error("同一个地址只报一次")
	}
	for i := range maxLinkDenied + 5 {
		bindLink("bind-test", "198.51.100."+strconv.Itoa(i+1))
	}
	linkBinds.Lock()
	n := len(linkBinds.m["bind-test"].denied)
	linkBinds.Unlock()
	if n > maxLinkDenied {
		t.Errorf("拒绝的地址记了 %d 个", n)
	}

	h := newLinkHarness(t)
	token, u := h.login(2)
	url := h.issue(u, volumeClaims)
	h.a.Auth.Logout(context.Background(), token)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = "198.51.100.200:1000"
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("退出后：%d", rec.Code)
	}
	id, err := openDownload(strings.TrimPrefix(url, downloadLinkPath), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	linkBinds.Lock()
	_, bound := linkBinds.m[id.ID]
	linkBinds.Unlock()
	if bound {
		t.Error("被拒绝的请求不应绑定链接")
	}
}

func TestOpenDownloadTooLong(t *testing.T) {
	if _, err := openDownload(strings.Repeat("A", 5000), time.Now()); err == nil {
		t.Error("太长的 token")
	}
}
