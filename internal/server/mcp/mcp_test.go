package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/oauth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 面板绑定域名时，请求由本机的 Caddy 转发：来源是本机，Host 是域名。这种请求要能正常使用 MCP。
func TestBehindLocalProxy(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, role) VALUES (1, 'admin', 'x', 0, 'admin')`); err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	secret := auth.NewSecret()
	if _, err := q.InsertAccessToken(ctx, store.AccessToken{UserID: 1, Name: "t", Source: auth.TokenSourcePersonal, TokenHash: auth.HashToken(secret), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	Mount(mux, Options{Handler: mux, Auth: auth.New(q, nil), OAuth: oauth.New(q, log), Log: log})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(body string, header map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Host = "panel.example.com"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+secret)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if st, body := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil); st != http.StatusOK || !strings.Contains(body, `"name":"nagi"`) {
		t.Fatalf("经本机代理、Host 是域名的请求应该能初始化：%d %s", st, body)
	}
	// 不保存会话：面板重启后客户端带着旧的会话 ID 来列工具，也要能拿到。
	if st, body := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": "gone-after-restart", "MCP-Protocol-Version": "2025-06-18"}); st != http.StatusOK || !strings.Contains(body, `"overview"`) {
		t.Fatalf("带着旧会话 ID 列工具应该成功：%d %s", st, body)
	}
}
