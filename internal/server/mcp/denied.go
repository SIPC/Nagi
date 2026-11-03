package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/SIPC/nagi/internal/server/auth"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// 被拒绝的工具调用记进活动记录（见 docs/mcp.md#安全）：令牌看不到的工具（没有修改、执行命令权限，不是管理员，
// 备用面板）客户端照样可以按名字调用，SDK 当作不存在的工具拒绝，到不了工具自己的检查，所以在这里先看一眼请求。

// maxPeek 是为了找工具名读的请求体上限；更大的请求原样交给 SDK，不看。
const maxPeek = 1 << 20

func (h *handler) recordDenied(ctx context.Context, u auth.User, tool, reason string) {
	if h.o.Record != nil {
		h.o.Record(ctx, u, tool, reason)
	}
}

// watchDenied 在交给 SDK 之前，找出请求里调用的、这个令牌不能用的工具。
func (h *handler) watchDenied(next http.Handler) http.Handler {
	byName := map[string]tool{}
	for _, t := range allTools {
		byName[t.name] = t
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := mcpauth.TokenInfoFromContext(r.Context())
		if r.Method != http.MethodPost || info == nil || r.ContentLength > maxPeek {
			next.ServeHTTP(w, r)
			return
		}
		u, _ := info.Extra[userExtra].(auth.User)
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		if err == nil && len(body) <= maxPeek {
			for _, name := range calledTools(body) {
				if t, ok := byName[name]; ok && !t.allowedFor(u, h.o.Standby) {
					h.recordDenied(r.Context(), u, name, "the access token is not allowed to use this tool")
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// calledTools 取出 JSON-RPC 请求（单个或批量）里 tools/call 的工具名。
func calledTools(body []byte) []string {
	type req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	var batch []req
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		_ = json.Unmarshal(body, &batch)
	} else {
		var one req
		if json.Unmarshal(body, &one) == nil {
			batch = []req{one}
		}
	}
	var out []string
	for _, x := range batch {
		if x.Method == "tools/call" && x.Params.Name != "" {
			out = append(out, x.Params.Name)
		}
	}
	return out
}
