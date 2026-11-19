package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"github.com/SIPC/nagi/web"
)

// 节点安装脚本、备用面板的状态和配对这几个不需要登录的地址，请求方法不对时返回 404，不落到前端页面：
// 开没开安全入口都一样（开了时，前端页面会暴露这是面板）。方法对的请求照常交给处理函数。
func TestPublicRoutesWrongMethod(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a := auth.New(store.New(d), nil)
	ok := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) })
	}
	build := func(primary bool) http.Handler {
		mux := http.NewServeMux()
		mux.Handle("GET /install.sh", ok("install")) // 和 api 包里一样
		var pair http.Handler
		if primary {
			pair = ok("pair")
		}
		mountPublicRoutes(mux, pair, ok("status"))
		mux.Handle("/", web.Handler())
		return a.EntranceMiddleware(mux)
	}
	type tc struct {
		method, path, want string // want 为空表示 404
	}
	check := func(h http.Handler, label string, cases []tc) {
		t.Helper()
		for _, c := range cases {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(c.method, "https://panel.example.com"+c.path, nil))
			if c.want == "" {
				if w.Code != http.StatusNotFound {
					t.Errorf("%s：%s %s 应该返回 404，得到 %d", label, c.method, c.path, w.Code)
				}
				continue
			}
			if w.Code != http.StatusOK || w.Body.String() != c.want {
				t.Errorf("%s：%s %s 应该交给处理函数，得到 %d %q", label, c.method, c.path, w.Code, w.Body.String())
			}
		}
	}
	wrong := []tc{
		{http.MethodPost, "/install.sh", ""},
		{http.MethodPut, "/install.sh", ""},
		{http.MethodDelete, "/install.sh", ""},
		{http.MethodPost, "/api/standby/status", ""},
		{http.MethodPut, "/api/standby/status", ""},
		{http.MethodGet, "/api/standby/pair", ""},
		{http.MethodPut, "/api/standby/pair", ""},
	}
	right := []tc{
		{http.MethodGet, "/install.sh", "install"},
		{http.MethodGet, "/api/standby/status", "status"},
		{http.MethodPost, "/api/standby/pair", "pair"},
	}

	for _, entrance := range []bool{false, true} {
		if entrance {
			if _, err := a.SetEntrance(ctx, true); err != nil {
				t.Fatal(err)
			}
		}
		label := map[bool]string{false: "没开安全入口", true: "开了安全入口"}[entrance]
		check(build(true), label, append(append([]tc{}, wrong...), right...))
		// 备用面板上没有配对接口。
		check(build(false), label+"的备用面板", []tc{{http.MethodPost, "/api/standby/pair", ""}, {http.MethodPost, "/install.sh", ""}})
	}
}
