package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/store"
)

// 授权页回调里的 iss（IssuerOf）要和 OAuth 元数据里的 issuer（访问的主机加 BasePath）完全一样，开不开安全入口都是。
func TestIssuerMatchesMetadata(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := New(store.New(d), nil)
	h := s.EntranceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			io.WriteString(w, "https://"+RequestHost(r)+BasePath(r.Context()))
			return
		}
		io.WriteString(w, s.IssuerOf(r.Context()))
	}))
	get := func(path string, cookie *http.Cookie) string {
		r := httptest.NewRequest(http.MethodGet, "https://panel.example.com"+path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	if meta, iss := get("/.well-known/oauth-authorization-server", nil), get("/nagi.v1.AuthService/ApproveOAuth", nil); meta != iss || iss != "https://panel.example.com" {
		t.Fatalf("没开安全入口：元数据 %q，iss %q", meta, iss)
	}
	path, err := s.SetEntrance(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	meta := get("/.well-known/oauth-authorization-server/"+path, nil)
	iss := get("/nagi.v1.AuthService/ApproveOAuth", &http.Cookie{Name: entranceCookie, Value: entranceToken(path)})
	if meta != iss || iss != "https://panel.example.com/"+path {
		t.Fatalf("开了安全入口：元数据 %q，iss %q", meta, iss)
	}
}

// 开了安全入口后，没登录的访问者只能访问 webhook、节点安装脚本和备用面板配对这几个地址，其余一律 404。
func TestEntranceHidesPanel(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := New(store.New(d), nil)
	if _, err := s.SetEntrance(ctx, true); err != nil {
		t.Fatal(err)
	}
	h := s.EntranceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "panel") }))
	for path, open := range map[string]bool{
		"/hooks/abc": true, "/install.sh": true, "/api/standby/status": true, "/dl/tok.ps1": true,
		"/dl": false, "/": false, "/login": false, "/download/nagi-linux-amd64": false, "/nagi.v1.AuthService/Me": false,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://panel.example.com"+path, nil))
		if got := w.Code != http.StatusNotFound; got != open {
			t.Errorf("%s：状态 %d，应该%s", path, w.Code, map[bool]string{true: "放行", false: "返回 404"}[open])
		}
	}

	// 放行的地址只认各自的请求方法，其余 404（后面会落到前端页面），带着入口 cookie 也一样。
	entered := &http.Cookie{Name: entranceCookie, Value: entranceToken(s.Entrance(ctx))}
	for _, c := range []struct {
		method, path string
		open         bool
	}{
		{http.MethodGet, "/install.sh", true},
		{http.MethodHead, "/install.sh", true},
		{http.MethodPost, "/install.sh", false},
		{http.MethodPut, "/install.sh", false},
		{http.MethodGet, "/api/standby/status", true},
		{http.MethodPost, "/api/standby/status", false},
		{http.MethodPost, "/api/standby/pair", true},
		{http.MethodGet, "/api/standby/pair", false},
		{http.MethodPost, "/hooks/abc", true},
	} {
		for _, cookie := range []*http.Cookie{nil, entered} {
			r := httptest.NewRequest(c.method, "https://panel.example.com"+c.path, nil)
			if cookie != nil {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Code != http.StatusNotFound; got != c.open {
				t.Errorf("%s %s（cookie %v）：状态 %d，应该%s", c.method, c.path, cookie != nil, w.Code, map[bool]string{true: "放行", false: "返回 404"}[c.open])
			}
		}
	}

	// L2：App 的接口只放行拿到令牌之前的四个，只认 POST 加 application/proto（App 发的格式）；其余和别处一样 404。
	for _, c := range []struct {
		method, path, contentType string
		open                      bool
	}{
		{http.MethodPost, "/nagi.v1.AppService/GetPairing", "application/proto", true},
		{http.MethodPost, "/nagi.v1.AppService/Pair", "application/proto", true},
		{http.MethodPost, "/nagi.v1.AppService/Challenge", "application/proto", true},
		{http.MethodPost, "/nagi.v1.AppService/Token", "application/proto", true},
		{http.MethodPost, "/nagi.v1.AppService/Token", "application/proto; charset=utf-8", true},
		{http.MethodGet, "/nagi.v1.AppService/Challenge", "", false},
		{http.MethodPost, "/nagi.v1.AppService/Challenge", "application/json", false},
		{http.MethodPost, "/nagi.v1.AppService/Challenge", "application/connect+proto", false},
		{http.MethodPost, "/nagi.v1.AppService/Challenge", "", false},
		{http.MethodPost, "/nagi.v1.AppService/Info", "application/proto", false},
		{http.MethodPost, "/nagi.v1.AppService/Logout", "application/proto", false},
		{http.MethodPost, "/nagi.v1.AppService/Nope", "application/proto", false},
		{http.MethodPost, "/nagi.v1.ProjectService/ListProjects", "application/connect+proto", false},
	} {
		r := httptest.NewRequest(c.method, "https://panel.example.com"+c.path, strings.NewReader(""))
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Code != http.StatusNotFound; got != c.open {
			t.Errorf("%s %s（%s）：状态 %d，应该%s", c.method, c.path, c.contentType, w.Code, map[bool]string{true: "放行", false: "返回 404"}[c.open])
		}
	}
}
