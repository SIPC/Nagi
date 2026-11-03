// Package mcp 让 agent 通过 MCP 查看和管理集群（见 docs/mcp.md）。
//
// 工具不直接调用业务代码：先按访问令牌核对成员，再在进程内调用面板自己的 Connect 接口，
// 角色、访问范围、备用面板只读和活动记录这些检查都由接口的拦截器照常完成。
package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/api"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/oauth"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options 是挂载 MCP 需要的东西。
type Options struct {
	// Handler 是面板的接口（同一个 mux），工具在进程内调用它。
	Handler http.Handler
	Auth    *auth.Service
	OAuth   *oauth.Server
	// Exec 在容器里执行一条命令（api.API.RunCommand）。
	Exec    func(ctx context.Context, projectID, container, service string, command []string, timeout time.Duration) (api.ExecResult, error)
	Version string
	// Standby 表示这是备用面板：只给查看类的工具。
	Standby bool
	Log     *slog.Logger
}

// 每个令牌每分钟的调用上限（见 docs/mcp.md#安全）。
const (
	callsPerMinute = 60
	execPerMinute  = 10
)

type handler struct {
	o       Options
	clients clients
	schemas *mcp.SchemaCache
	http    *mcp.StreamableHTTPHandler

	mu    sync.Mutex
	calls map[int64][]callStamp
}

type callStamp struct {
	at   time.Time
	exec bool
}

// clients 是进程内调用面板接口的 Connect 客户端。
type clients struct {
	projects nagiv1connect.ProjectServiceClient
	nodes    nagiv1connect.NodeServiceClient
}

// Mount 挂上 /mcp 和 OAuth 的地址。
func Mount(mux *http.ServeMux, o Options) {
	h := &handler{o: o, schemas: mcp.NewSchemaCache(), calls: map[int64][]callStamp{}}
	hc := &http.Client{Transport: inProcess{o.Handler}}
	const base = "http://nagi.internal"
	h.clients = clients{
		projects: nagiv1connect.NewProjectServiceClient(hc, base),
		nodes:    nagiv1connect.NewNodeServiceClient(hc, base),
	}
	h.http = mcp.NewStreamableHTTPHandler(h.server, &mcp.StreamableHTTPOptions{
		Logger: o.Log,
		// SDK 默认拒绝「从本机地址进来、Host 不是 localhost」的请求（防 DNS 重绑定）。面板绑定域名时请求都由本机的
		// Caddy 转发，正是这种情况。/mcp 只认请求头里的令牌，浏览器不会自动带上，DNS 重绑定拿不到权限，所以关掉。
		DisableLocalhostProtection: true,
	})
	mux.Handle("/mcp", h)
	o.OAuth.Mount(mux)
}

// ServeHTTP 核对令牌后交给 SDK。没带令牌或令牌无效时返回 401，告诉客户端去哪里授权（OAuth 的发现流程）。
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
	w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	verify := func(ctx context.Context, token string, r *http.Request) (*mcpauth.TokenInfo, error) {
		u, t, err := h.o.Auth.TokenUser(ctx, token, auth.RequestIP(r.RemoteAddr, r.Header))
		if errors.Is(err, auth.ErrInvalidToken) {
			return nil, mcpauth.ErrInvalidToken
		} else if err != nil {
			return nil, err
		}
		info := &mcpauth.TokenInfo{UserID: strconv.FormatInt(u.ID, 10), Extra: map[string]any{userExtra: u}}
		if t.ExpiresAt > 0 {
			info.Expiration = time.Unix(t.ExpiresAt, 0)
		}
		return info, nil
	}
	mw := mcpauth.RequireBearerToken(verify, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: "https://" + auth.RequestHost(r) + "/.well-known/oauth-protected-resource" + auth.BasePath(r.Context()) + "/mcp",
		// 个人访问令牌可以不过期。
		AllowMissingExpiration: true,
	})
	mw(h.http).ServeHTTP(w, r)
}

const userExtra = "nagi.user"

// userOf 取请求对应的成员。每次请求都重新核对令牌，成员的角色、范围改了立即生效。
func userOf(req *mcp.CallToolRequest) (auth.User, bool) {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return auth.User{}, false
	}
	u, ok := req.Extra.TokenInfo.Extra[userExtra].(auth.User)
	return u, ok
}

// allow 按令牌限制调用频率。
func (h *handler) allow(u auth.User, exec bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	kept := h.calls[u.TokenID][:0]
	n, nExec := 0, 0
	for _, c := range h.calls[u.TokenID] {
		if now.Sub(c.at) < time.Minute {
			kept = append(kept, c)
			n++
			if c.exec {
				nExec++
			}
		}
	}
	if n >= callsPerMinute || exec && nExec >= execPerMinute {
		h.calls[u.TokenID] = kept
		return false
	}
	h.calls[u.TokenID] = append(kept, callStamp{at: now, exec: exec})
	return true
}

// ---- 进程内调用 ----

// inProcess 把 Connect 客户端的请求直接交给面板的接口，不经过网络。请求的 ctx 里带着按令牌核对过的成员
// （auth.WithInternalUser），登录检查直接采用它。
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &pipeResponse{header: http.Header{}, body: pw, ready: make(chan struct{})}
	srv := req.Clone(req.Context())
	srv.RemoteAddr = "127.0.0.1:0"
	if lang, ok := req.Context().Value(langKey{}).(string); ok {
		srv.Header.Set("Accept-Language", lang)
	}
	srv.RequestURI = req.URL.RequestURI()
	if srv.Body == nil {
		srv.Body = http.NoBody
	}
	go func() {
		defer func() {
			w.WriteHeader(http.StatusOK)
			pw.Close()
		}()
		t.h.ServeHTTP(w, srv)
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status: http.StatusText(w.status), StatusCode: w.status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: w.sent, Body: pr, ContentLength: -1, Request: req,
	}, nil
}

type pipeResponse struct {
	header http.Header
	sent   http.Header
	status int
	body   *io.PipeWriter
	once   sync.Once
	ready  chan struct{}
}

func (w *pipeResponse) Header() http.Header { return w.header }

func (w *pipeResponse) WriteHeader(code int) {
	w.once.Do(func() {
		w.status = code
		w.sent = w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

func (w *pipeResponse) Flush() {}

type langKey struct{}

// callCtx 是调用面板接口用的 ctx：带着成员和客户端的语言（错误信息按它返回）。
func callCtx(ctx context.Context, u auth.User, req *mcp.CallToolRequest) context.Context {
	lang := "zh-CN"
	if req.Extra != nil && req.Extra.Header != nil && req.Extra.Header.Get("Accept-Language") != "" {
		lang = req.Extra.Header.Get("Accept-Language")
	}
	return context.WithValue(auth.WithInternalUser(ctx, u), langKey{}, lang)
}
