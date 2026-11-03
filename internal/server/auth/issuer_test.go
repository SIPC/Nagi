package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
