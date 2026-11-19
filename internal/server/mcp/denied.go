package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
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

// record 记一次工具调用（见 docs/audit.md#mcp-调用）。ctx 是调用面板接口用的（callCtx），errMsg 是带节点引用标记的
// 失败详情（见 api.FailureNotes）。
//
// 记录以后要给看不到某些节点的成员看，所以写的时候就把节点处理成能按人换的形式，显示时不用猜：
// 节点参数写成 ID（nodeArgIDs），失败详情带引用标记；perms 记下用这个工具要的权限，没有这些权限的成员看不到
// 参数和失败详情。有 perms 的记录就是按这个办法写的（api 的 mcpPerms 据此判断）。
func (h *handler) record(ctx context.Context, u auth.User, t tool, args []byte, result, errMsg string, bytes int, d time.Duration, col *audit.Collector) {
	if h.o.Audit == nil {
		return
	}
	args, nodes := h.nodeArgIDs(ctx, args)
	params := map[string]any{"perms": t.permNames()}
	if p := audit.JSONParams(args); p != "" {
		params["arguments"] = json.RawMessage(p)
	}
	if bytes > 0 {
		params["result_bytes"] = bytes
	}
	b, _ := json.Marshal(params)
	if len(b) > audit.MaxParams {
		// 整体再截断一次的话 perms 会落进截断后的文字里认不出来：参数只留「已截断」。
		params["arguments"] = map[string]any{"truncated": true}
		b, _ = json.Marshal(params)
	}
	e := audit.Event{Category: audit.MCP, Action: "mcp:" + t.name, TargetType: "tool", TargetID: t.name, TargetName: t.name,
		Params: string(b), Result: result, Error: errMsg, DurationMS: d.Milliseconds()}
	var in struct {
		Project   string `json:"project"`
		Container string `json:"container"`
		Node      string `json:"node"`
	}
	_ = json.Unmarshal(args, &in)
	if in.Container != "" {
		e.TargetType, e.TargetID, e.TargetName = "container", in.Container, in.Container
	} else if in.Node != "" {
		// 认得出的节点记 ID 和名称（显示时按 ID 判断查看的人看不看得到）；认不出的是调用者自己写的文字。
		e.TargetType, e.TargetID, e.TargetName = "node", in.Node, in.Node
		if name, ok := nodes[in.Node]; ok {
			e.TargetName = name
		}
	}
	if col == nil {
		_, col = audit.WithCollector(ctx)
	}
	h.o.Audit(ctx, u, e, in.Project, col)
}

// nodeArg 判断工具参数的键是不是用来指定节点的（名称或 ID）：键名里有 node，或者是 peer。
// 新加的参数要照这个规则起名，记录时才会换成 ID（TestNodeArgKeys 检查）。
func nodeArg(key string) bool {
	k := strings.ToLower(key)
	return k == "peer" || strings.Contains(k, "node")
}

// nodeArgIDs 把参数里按名称写的节点换成节点 ID，返回换过的参数和换到的节点（ID → 名称）。
// 名称按调用者看得到的节点列表对（和 findNode 一样不分大小写），重名的写成各台 ID 的数组（每个 ID 单独一项，
// 查看记录的成员看不到其中某台时，那一项整个去掉、成为空字符串，不会剩下半截）；对不上的原样留着：
// 那是调用者自己写的文字（写错的名称，或者他看不到的节点）；拿不到节点列表时一律清空。记成 ID 以后，查看记录的成员看不到的节点由面板按 ID 去掉，
// 名称不会留在记录里。
func (h *handler) nodeArgIDs(ctx context.Context, args []byte) ([]byte, map[string]string) {
	var m map[string]any
	// 数字照原样留着（修订号这类大整数不能经过 float64）。
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return args, nil
	}
	var list []*nagiv1.Node
	loaded, listed := false, false
	found := map[string]string{}
	// resolve 返回换成的值（ID，或者重名时 ID 的列表）和换没换。
	resolve := func(ref string) (any, bool) {
		if ref == "" {
			return ref, false
		}
		if !loaded {
			loaded = true
			if h.clients.nodes != nil {
				if res, err := h.clients.nodes.ListNodes(ctx, connect.NewRequest(&nagiv1.ListNodesRequest{})); err == nil {
					list, listed = res.Msg.GetNodes(), true
				}
			}
		}
		// 拿不到节点列表（调用者没有「查看节点」，或者一时出错）就对不了：不留原文，免得名称留在记录里。
		if !listed {
			return "", true
		}
		for _, n := range list {
			if n.GetId() == ref {
				found[n.GetId()] = n.GetName()
				return ref, false
			}
		}
		var ids []string
		for _, n := range list {
			if strings.EqualFold(n.GetName(), ref) {
				ids = append(ids, n.GetId())
				found[n.GetId()] = n.GetName()
			}
		}
		switch len(ids) {
		case 0:
			return ref, false
		case 1:
			return ids[0], true
		}
		return ids, true
	}
	changed := false
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if !nodeArg(k) {
					walk(x)
					continue
				}
				switch val := x.(type) {
				case string:
					if id, ok := resolve(val); ok {
						t[k], changed = id, true
					}
				case []any:
					for i, item := range val {
						if s, ok := item.(string); ok {
							if id, ok := resolve(s); ok {
								val[i], changed = id, true
							}
						}
					}
				}
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(m)
	if !changed {
		return args, found
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args, found
	}
	return b, found
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
