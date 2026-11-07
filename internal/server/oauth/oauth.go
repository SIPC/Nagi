// Package oauth 是面板自己的 OAuth 2.1 授权服务器，给只支持 OAuth 的 MCP 客户端用（见 docs/mcp.md#oauth）。
// 只支持公开客户端（不用客户端密钥）、授权码加 PKCE（S256）和刷新令牌。授权后发出的令牌和个人访问令牌存在同一张表里。
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

const (
	// AccessTTL 是 OAuth 访问令牌的有效期，过期后用刷新令牌换新的。
	AccessTTL = time.Hour
	codeTTL   = 5 * time.Minute
	// maxClients 是动态注册的客户端上限；24 小时没被授权的由 auth.CleanupLoop 删掉。
	maxClients = 100
	// registerPerHour 是每个来源地址每小时能注册的客户端数。
	registerPerHour = 10
	// MaxTokens 是每个成员的访问令牌上限（个人令牌和 OAuth 授权一起算）。
	MaxTokens = 20
	maxBody   = 16 << 10
)

// 令牌的范围：读、修改、执行命令。授权页上由成员选，客户端请求的范围不起作用。
const (
	ScopeRead  = "nagi:read"
	ScopeWrite = "nagi:write"
	ScopeExec  = "nagi:exec"
	// ScopeNode：在节点上执行命令（node_exec）。
	ScopeNode = "nagi:node"
)

// Server 是授权服务器。授权码只放在内存里：面板重启时进行到一半的授权要重新来。
type Server struct {
	Q   *store.Queries
	Log *slog.Logger

	mu       sync.Mutex
	codes    map[string]grant
	register map[string][]time.Time
	docs     map[string]cachedDoc
	fetch    *http.Client
}

// New 创建授权服务器。
func New(q *store.Queries, log *slog.Logger) *Server {
	return &Server{Q: q, Log: log, codes: map[string]grant{}, register: map[string][]time.Time{}, docs: map[string]cachedDoc{}, fetch: safeClient()}
}

type grant struct {
	client     Client
	redirect   string
	challenge  string
	resource   string
	userID     int64
	allowWrite bool
	allowExec  bool
	allowNode  bool
	days       uint32
	expires    time.Time
}

// Client 是核对过的客户端：动态注册的，或者用网址作标识的（客户端元数据文档）。
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// BaseURL 是授权服务器的地址（issuer）：面板地址，经安全入口访问时带着入口（见 docs/mcp.md#入口）。
func BaseURL(r *http.Request) string {
	return "https://" + auth.RequestHost(r) + auth.BasePath(r.Context())
}

// ResourceURL 是 MCP 的地址，令牌只能用于它。
func ResourceURL(r *http.Request) string { return BaseURL(r) + "/mcp" }

// sameResource 比较客户端给的 resource 和本面板的 MCP 地址：客户端照着用户填的地址传，末尾可能多一个 /。
func sameResource(got, want string) bool {
	return strings.TrimSuffix(got, "/") == want
}

func scopesOf(write, exec, node bool) string {
	s := []string{ScopeRead}
	if write {
		s = append(s, ScopeWrite)
	}
	if exec {
		s = append(s, ScopeExec)
	}
	if node {
		s = append(s, ScopeNode)
	}
	return strings.Join(s, " ")
}

// ---- 客户端 ----

func (s *Server) client(ctx context.Context, id string) (Client, error) {
	if strings.HasPrefix(id, "https://") {
		return s.metadataClient(ctx, id)
	}
	c, err := s.Q.GetOAuthClient(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, i18n.New("客户端未注册")
	} else if err != nil {
		return Client{}, err
	}
	var uris []string
	if err := json.Unmarshal([]byte(c.RedirectURIs), &uris); err != nil {
		return Client{}, err
	}
	return Client{ID: c.ID, Name: c.Name, RedirectURIs: uris}, nil
}

// validRedirect 判断能不能用作回调地址：https、本机的 http，或者本机应用自己的协议（RFC 8252）。
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return loopback(u.Hostname())
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return false
	}
	return strings.Contains(u.Scheme, ".") || u.Host != "" || u.Path != ""
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// matchRedirect：回调地址必须和注册时的一致。本机的 http 地址可以换端口（RFC 8252 7.3），客户端每次随机选端口。
func matchRedirect(registered []string, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, r := range registered {
		if r == raw {
			return true
		}
		ru, err := url.Parse(r)
		if err == nil && u.Scheme == "http" && ru.Scheme == "http" && loopback(u.Hostname()) && u.Hostname() == ru.Hostname() &&
			u.Path == ru.Path && u.RawQuery == ru.RawQuery {
			return true
		}
	}
	return false
}

// ---- 授权页 ----

// Check 核对授权请求。客户端或回调地址不对时不能转回客户端（可能是钓鱼），由授权页直接显示错误。
func (s *Server) Check(ctx context.Context, req *nagiv1.OAuthRequest) (Client, error) {
	c, err := s.client(ctx, req.GetClientId())
	if err != nil {
		return c, err
	}
	if !matchRedirect(c.RedirectURIs, req.GetRedirectUri()) {
		return c, i18n.New("回调地址与客户端注册的不一致")
	}
	if req.GetResponseType() != "code" {
		return c, i18n.New("只支持授权码方式")
	}
	if req.GetCodeChallenge() == "" || req.GetCodeChallengeMethod() != "S256" {
		return c, i18n.New("客户端须使用 PKCE（S256）")
	}
	return c, nil
}

// RedirectHost 是授权页上显示的回调地址的域名。
func RedirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host != "" {
		return u.Hostname()
	}
	return u.Scheme + ":"
}

// Approve 处理成员在授权页上的选择，返回浏览器要转去的回调地址。issuer 写进回调地址的 iss（RFC 9207），
// 客户端据此确认授权码来自它找的授权服务器；ChatGPT 只有服务器支持 iss 时才用它登记过的固定回调地址。
func (s *Server) Approve(ctx context.Context, u auth.User, req *nagiv1.OAuthRequest, issuer string, allow bool, days uint32, write, exec, node bool) (string, error) {
	c, err := s.Check(ctx, req)
	if err != nil {
		return "", err
	}
	q := url.Values{"iss": {issuer}}
	if req.GetState() != "" {
		q.Set("state", req.GetState())
	}
	if !allow {
		q.Set("error", "access_denied")
		return withQuery(req.GetRedirectUri(), q), nil
	}
	if n, err := s.Q.CountAccessTokens(ctx, u.ID); err != nil {
		return "", err
	} else if n >= MaxTokens {
		return "", i18n.Errorf("访问令牌已达上限（%d 个），请先吊销不再使用的令牌", MaxTokens)
	}
	if u.Role == auth.RoleViewer {
		write, exec = false, false
	}
	// 节点终端等于节点的 root：只有管理员能授权，而且要同时允许执行命令（和个人访问令牌一样）。
	if node && u.Role != auth.RoleAdmin {
		return "", i18n.New("只有管理员能创建可在节点上执行命令的令牌")
	}
	if node && !exec {
		return "", i18n.New("允许节点终端时须同时允许执行命令")
	}
	code := randomHex(32)
	s.mu.Lock()
	s.prune()
	s.codes[code] = grant{client: c, redirect: req.GetRedirectUri(), challenge: req.GetCodeChallenge(), resource: req.GetResource(),
		userID: u.ID, allowWrite: write, allowExec: exec, allowNode: node, days: days, expires: time.Now().Add(codeTTL)}
	s.mu.Unlock()
	if !strings.HasPrefix(c.ID, "https://") {
		_ = s.Q.MarkOAuthClientAuthorized(ctx, c.ID, time.Now().Unix())
	}
	q.Set("code", code)
	return withQuery(req.GetRedirectUri(), q), nil
}

func withQuery(raw string, q url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	v := u.Query()
	for k, vs := range q {
		v[k] = vs
	}
	u.RawQuery = v.Encode()
	return u.String()
}

// prune 删掉过期的授权码。调用时持有 mu。
func (s *Server) prune() {
	now := time.Now()
	for k, g := range s.codes {
		if now.After(g.expires) {
			delete(s.codes, k)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---- HTTP ----

// Mount 挂上元数据、注册、令牌和吊销的地址。授权页（/oauth/authorize）是网页，由前端处理。
func (s *Server) Mount(mux *http.ServeMux) {
	resource := http.HandlerFunc(s.handleResourceMetadata)
	mux.Handle("/.well-known/oauth-protected-resource", resource)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", resource)
	server := http.HandlerFunc(s.handleServerMetadata)
	mux.Handle("/.well-known/oauth-authorization-server", server)
	mux.Handle("/.well-known/openid-configuration", server)
	mux.HandleFunc("/oauth/register", s.handleRegister)
	mux.HandleFunc("/oauth/token", s.handleToken)
	mux.HandleFunc("/oauth/revoke", s.handleRevoke)
}

// cors：这些地址是给客户端程序调用的，没有 cookie，任何来源都可以调用（浏览器里的客户端也能用）。
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	writeJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

func (s *Server) handleResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 ResourceURL(r),
		"authorization_servers":    []string{BaseURL(r)},
		"scopes_supported":         []string{ScopeRead, ScopeWrite, ScopeExec, ScopeNode},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Nagi",
	})
}

func (s *Server) handleServerMetadata(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	base := BaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + "/oauth/authorize",
		"token_endpoint":                                 base + "/oauth/token",
		"registration_endpoint":                          base + "/oauth/register",
		"revocation_endpoint":                            base + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{ScopeRead, ScopeWrite, ScopeExec, ScopeNode},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	// 经面板自带的 Caddy 转发时来源都是本机，要用它转发来的客户端地址，否则所有人共用一份注册限额。
	ip := auth.RequestIP(r.RemoteAddr, r.Header)
	s.mu.Lock()
	recent := s.register[ip][:0]
	for _, t := range s.register[ip] {
		if time.Since(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	limited := len(recent) >= registerPerHour
	if !limited {
		recent = append(recent, time.Now())
	}
	s.register[ip] = recent
	s.mu.Unlock()
	if limited {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many registrations, try again later")
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "1 to 10 redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) || len(u) > 2000 {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be https, a loopback http address or a private-use scheme: "+u)
			return
		}
	}
	name := clip(strings.TrimSpace(req.ClientName), 64)
	if name == "" {
		name = "MCP client"
	}
	n, err := s.Q.CountOAuthClients(r.Context())
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if n >= maxClients {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients")
		return
	}
	uris, _ := json.Marshal(req.RedirectURIs)
	now := time.Now()
	id := randomHex(16)
	if err := s.Q.InsertOAuthClient(r.Context(), store.OAuthClient{ID: id, Name: name, RedirectURIs: string(uris), CreatedAt: now.Unix()}); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        now.Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

func clip(v string, n int) string {
	r := []rune(v)
	if len(r) > n {
		return string(r[:n])
	}
	return v
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" && !sameResource(res, ResourceURL(r)) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "this server only issues tokens for "+ResourceURL(r))
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

// pkceOK 核对 PKCE：S256(code_verifier) 要等于授权时的 code_challenge。
func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	s.mu.Lock()
	g, ok := s.codes[f.Get("code")]
	// 授权码只能用一次，不论这次是否成功。
	delete(s.codes, f.Get("code"))
	s.mu.Unlock()
	switch {
	case !ok || time.Now().After(g.expires):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	case f.Get("client_id") != g.client.ID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match")
		return
	case f.Get("redirect_uri") != g.redirect:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
		return
	case !pkceOK(f.Get("code_verifier"), g.challenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	case g.resource != "" && !sameResource(g.resource, ResourceURL(r)):
		oauthError(w, http.StatusBadRequest, "invalid_target", "this server only issues tokens for "+ResourceURL(r))
		return
	}
	now := time.Now()
	access, refresh := auth.NewSecret(), auth.NewSecret()
	t := store.AccessToken{
		UserID: g.userID, Name: g.client.Name, Source: auth.TokenSourceOAuth, ClientID: g.client.ID,
		TokenHash: auth.HashToken(access), RefreshHash: auth.HashToken(refresh),
		AllowWrite: g.allowWrite, AllowExec: g.allowExec, AllowNodeExec: g.allowNode, CreatedAt: now.Unix(), ExpiresAt: now.Add(AccessTTL).Unix(),
	}
	if g.days > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(g.days) * 24 * time.Hour).Unix()
	}
	if _, err := s.Q.InsertAccessToken(r.Context(), t); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	s.Log.Info("OAuth 授权已发出令牌", "client", g.client.Name, "user", g.userID)
	writeTokens(w, access, refresh, g.allowWrite, g.allowExec, g.allowNode)
}

func writeTokens(w http.ResponseWriter, access, refresh string, write, exec, node bool) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(AccessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         scopesOf(write, exec, node),
	})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hash := auth.HashToken(r.PostForm.Get("refresh_token"))
	t, err := s.Q.AccessTokenByRefresh(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		// 用过的刷新令牌又出现了：可能被人偷去用了，吊销整个授权，让成员重新授权。
		if old, err := s.Q.AccessTokenByPrevRefresh(ctx, hash); err == nil {
			_, _ = s.Q.DeleteAccessToken(ctx, old.ID)
			s.Log.Warn("OAuth 刷新令牌被重复使用，已吊销这个授权", "client", old.Name, "user", old.UserID)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid")
		return
	} else if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	now := time.Now()
	switch {
	case t.ClientID != r.PostForm.Get("client_id"):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match")
		return
	case t.RefreshExpiresAt > 0 && t.RefreshExpiresAt < now.Unix():
		_, _ = s.Q.DeleteAccessToken(ctx, t.ID)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization has expired")
		return
	}
	access, refresh := auth.NewSecret(), auth.NewSecret()
	if err := s.Q.RotateAccessToken(ctx, t.ID, auth.HashToken(access), now.Add(AccessTTL).Unix(), auth.HashToken(refresh), hash); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeTokens(w, access, refresh, t.AllowWrite, t.AllowExec, t.AllowNodeExec)
}

// handleRevoke 吊销访问令牌或刷新令牌（RFC 7009）：不论令牌是否存在都返回成功。
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	hash := auth.HashToken(r.PostForm.Get("token"))
	t, err := s.Q.AccessTokenByHash(r.Context(), hash)
	if err != nil {
		t, err = s.Q.AccessTokenByRefresh(r.Context(), hash)
	}
	if err == nil && t.Source == auth.TokenSourceOAuth {
		_, _ = s.Q.DeleteAccessToken(r.Context(), t.ID)
	}
	w.WriteHeader(http.StatusOK)
}

// ---- 用网址作客户端标识（客户端元数据文档）----

type cachedDoc struct {
	client  Client
	err     error
	expires time.Time
}

// metadataClient 取客户端元数据文档。网址由客户端提供，面板去访问它，所以只能访问公网地址（见 safeClient）。
func (s *Server) metadataClient(ctx context.Context, id string) (Client, error) {
	s.mu.Lock()
	if d, ok := s.docs[id]; ok && time.Now().Before(d.expires) {
		s.mu.Unlock()
		return d.client, d.err
	}
	s.mu.Unlock()
	c, err := s.fetchMetadata(ctx, id)
	s.mu.Lock()
	if len(s.docs) > 200 {
		s.docs = map[string]cachedDoc{}
	}
	s.docs[id] = cachedDoc{client: c, err: err, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return c, err
}

func (s *Server) fetchMetadata(ctx context.Context, id string) (Client, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.Path == "/" {
		return Client{}, i18n.New("客户端标识须为网址")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return Client{}, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.fetch.Do(req)
	if err != nil {
		return Client{}, i18n.Detail(i18n.New("无法获取客户端元数据，请检查客户端标识的网址"), err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Client{}, i18n.Errorf("无法获取客户端元数据（%s）", res.Status)
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&doc); err != nil {
		return Client{}, i18n.New("客户端元数据须为 JSON")
	}
	if doc.ClientID != id {
		return Client{}, i18n.New("客户端元数据中的 client_id 与网址不一致")
	}
	for _, r := range doc.RedirectURIs {
		if !validRedirect(r) {
			return Client{}, i18n.New("客户端元数据中有不可用的回调地址")
		}
	}
	name := clip(strings.TrimSpace(doc.ClientName), 64)
	if name == "" {
		name = u.Hostname()
	}
	return Client{ID: id, Name: name, RedirectURIs: doc.RedirectURIs}, nil
}

// safeClient 只访问公网地址：解析后的 IP 是本机、内网、链路本地这些的一律拒绝，免得被用来探测面板所在的内网。
func safeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if publicIP(ip.IP) {
						return dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					}
				}
				return nil, errors.New("only public addresses are allowed")
			},
		},
	}
}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		// 100.64.0.0/10（运营商 NAT）
		ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)
}
