package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

func testServer(t *testing.T) (*Server, *auth.Service, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, role) VALUES (2, 'op', 'x', 0, 'operator')`); err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	s := New(q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	s.Mount(mux)
	hs := httptest.NewTLSServer(mux)
	t.Cleanup(hs.Close)
	return s, auth.New(q, nil), hs
}

func post(t *testing.T, hs *httptest.Server, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	res, err := hs.Client().PostForm(hs.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func register(t *testing.T, hs *httptest.Server, redirect string) string {
	t.Helper()
	res, err := hs.Client().Post(hs.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+redirect+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("注册失败：%d %v", res.StatusCode, out)
	}
	return out["client_id"].(string)
}

const verifier = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorize(t *testing.T, s *Server, client, redirect string, write bool) string {
	t.Helper()
	req := &nagiv1.OAuthRequest{ResponseType: "code", ClientId: client, RedirectUri: redirect, State: "xyz",
		CodeChallenge: challenge(verifier), CodeChallengeMethod: "S256"}
	to, err := s.Approve(context.Background(), auth.User{ID: 2, Role: auth.RoleOperator}, req, true, 30, write, false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(to)
	if err != nil || u.Query().Get("state") != "xyz" || u.Query().Get("code") == "" {
		t.Fatalf("回调地址不对：%s", to)
	}
	return u.Query().Get("code")
}

func TestOAuthFlow(t *testing.T) {
	s, a, hs := testServer(t)
	ctx := context.Background()
	redirect := "https://claude.ai/api/mcp/auth_callback"
	client := register(t, hs, redirect)

	// 回调地址不一致、没有 PKCE 时授权页直接报错，不转回客户端。
	if _, err := s.Check(ctx, &nagiv1.OAuthRequest{ResponseType: "code", ClientId: client, RedirectUri: "https://evil.example/cb", CodeChallenge: "x", CodeChallengeMethod: "S256"}); err == nil {
		t.Error("回调地址不一致时应该拒绝")
	}
	if _, err := s.Check(ctx, &nagiv1.OAuthRequest{ResponseType: "code", ClientId: client, RedirectUri: redirect}); err == nil {
		t.Error("没有 PKCE 时应该拒绝")
	}

	// PKCE 不对：失败，授权码也作废。
	code := authorize(t, s, client, redirect, true)
	if st, _ := post(t, hs, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {client},
		"redirect_uri": {redirect}, "code_verifier": {strings.Repeat("x", 43)}}); st != http.StatusBadRequest {
		t.Errorf("PKCE 不对时应该拒绝：%d", st)
	}
	good := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	if st, _ := post(t, hs, "/oauth/token", good); st != http.StatusBadRequest {
		t.Errorf("用过的授权码应该作废：%d", st)
	}

	// 正常换令牌。
	code = authorize(t, s, client, redirect, true)
	good.Set("code", code)
	st, tok := post(t, hs, "/oauth/token", good)
	if st != http.StatusOK || tok["access_token"] == nil || tok["scope"] != ScopeRead+" "+ScopeWrite {
		t.Fatalf("换令牌失败：%d %v", st, tok)
	}
	u, _, err := a.TokenUser(ctx, tok["access_token"].(string), "1.2.3.4")
	if err != nil || u.ID != 2 || !u.AllowWrite || u.AllowExec || u.Via != "mcp:Claude" {
		t.Fatalf("令牌对应的成员不对：%+v %v", u, err)
	}

	// 刷新：换一对新的，旧的访问令牌失效。
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {client}}
	st, tok2 := post(t, hs, "/oauth/token", refresh)
	if st != http.StatusOK || tok2["access_token"] == tok["access_token"] {
		t.Fatalf("刷新失败：%d %v", st, tok2)
	}
	if _, _, err := a.TokenUser(ctx, tok["access_token"].(string), ""); err == nil {
		t.Error("刷新后旧的访问令牌应该失效")
	}
	// 旧的刷新令牌又被用了：吊销整个授权。
	if st, _ := post(t, hs, "/oauth/token", refresh); st != http.StatusBadRequest {
		t.Errorf("旧的刷新令牌应该被拒绝：%d", st)
	}
	if _, _, err := a.TokenUser(ctx, tok2["access_token"].(string), ""); err == nil {
		t.Error("刷新令牌被重复使用后，整个授权应该被吊销")
	}

	// 吊销。
	code = authorize(t, s, client, redirect, false)
	good.Set("code", code)
	_, tok3 := post(t, hs, "/oauth/token", good)
	if st, _ := post(t, hs, "/oauth/revoke", url.Values{"token": {tok3["refresh_token"].(string)}}); st != http.StatusOK {
		t.Errorf("吊销失败：%d", st)
	}
	if _, _, err := a.TokenUser(ctx, tok3["access_token"].(string), ""); err == nil {
		t.Error("吊销后令牌应该失效")
	}
}

func TestOAuthDeny(t *testing.T) {
	s, _, hs := testServer(t)
	redirect := "http://127.0.0.1:33418/callback"
	client := register(t, hs, redirect)
	// 本机回调可以换端口。
	req := &nagiv1.OAuthRequest{ResponseType: "code", ClientId: client, RedirectUri: "http://127.0.0.1:50000/callback", State: "s",
		CodeChallenge: challenge(verifier), CodeChallengeMethod: "S256"}
	to, err := s.Approve(context.Background(), auth.User{ID: 2, Role: auth.RoleOperator}, req, false, 0, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(to); u.Query().Get("error") != "access_denied" || u.Query().Get("code") != "" {
		t.Errorf("拒绝时应该带 access_denied：%s", to)
	}
}

func TestRedirectRules(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://localhost:8080/cb":                true,
		"http://127.0.0.1/cb":                     true,
		"http://example.com/cb":                   false,
		"cursor://anysphere.cursor-retrieval/cb":  true,
		"javascript:alert(1)":                     false,
		"https://a.example/cb#frag":               false,
	} {
		if got := validRedirect(raw); got != want {
			t.Errorf("validRedirect(%q) = %v，想要 %v", raw, got, want)
		}
	}
}

func TestMetadata(t *testing.T) {
	_, _, hs := testServer(t)
	res, err := hs.Client().Get(hs.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	host := strings.TrimPrefix(hs.URL, "https://")
	if m["issuer"] != "https://"+host || m["token_endpoint"] != "https://"+host+"/oauth/token" {
		t.Errorf("元数据不对：%v", m)
	}
}
