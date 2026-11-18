package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// revokeDirectLinks 在成员的权限被撤销后调用：给各节点换直连下载的密钥，之前签的链接立即失效
// （链接不绑定用户，见 nodes.Registry.RotateDirect）。在后台进行，不拖慢请求；正在换时再调用，
// 换完以后再换一次（合并成一次），不同时发很多次。
func (a *API) revokeDirectLinks() {
	if a.Nodes == nil {
		return
	}
	r := &a.directRot
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		r.again = true
		return
	}
	r.running = true
	go func() {
		for {
			rotateDirect(a.Nodes)
			r.mu.Lock()
			if !r.again {
				r.running = false
				r.mu.Unlock()
				return
			}
			r.again = false
			r.mu.Unlock()
		}
	}()
}

type directRotation struct {
	mu      sync.Mutex
	running bool
	again   bool
}

// rotateDirect 是 Registry.RotateDirect，测试里换掉。
var rotateDirect = func(r *nodes.Registry) { r.RotateDirect() }

// 直连下载（见 docs/agent.md#直连下载）：权限和经面板下载一样在这里检查，通过后签一个 5 分钟有效的链接，
// 浏览器拿着它直接从节点下载。节点不能直连时回 204，浏览器照旧经面板下载。签发链接时就记进活动记录，
// 带上链接的编号（direct_link）：之后的下载不经面板，由节点记进它的本地审计（同一个编号），随节点的审计外送。
// 撤销成员的权限时面板给各节点换密钥，没过期的链接也立即失效（revokeDirectLinks）。

func writeDirect(w http.ResponseWriter, url string) {
	if url == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
}

// writeCommand 回答加密 HTTP 下载的命令（mode=command，见 docs/agent.md#加密-http-下载）。
// 节点不能用时回 409。命令里有内容密钥：不缓存，也不写进活动记录。
func writeCommand(w http.ResponseWriter, r *http.Request, cmd *nodes.DirectHTTPCommand) {
	if cmd == nil {
		jsonFail(w, r, http.StatusConflict, i18n.New("该节点现在不能生成下载命令"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"command": cmd.Command, "command_windows": cmd.CommandWindows, "segmented": cmd.Segmented, "name": cmd.Name, "size": cmd.Size, "archive": cmd.Archive,
		"sha256": cmd.SHA256, "hash_skipped": cmd.HashSkipped, "expires": cmd.Expires.Unix(),
	})
}

// commandMode：请求要的是加密 HTTP 下载的命令（mode=command），不是 HTTPS 的直连链接。
func commandMode(r *http.Request) bool { return r.URL.Query().Get("mode") == "command" }

func jsonFail(w http.ResponseWriter, r *http.Request, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": errorText(requestLang(r), err)})
}

// 直连下载的方式：HTTPS 链接、加密 HTTP 下载的命令，或者能用哪种给哪种（MCP，HTTPS 优先）。
type directMode int

const (
	directLinkMode directMode = iota
	directCommandMode
	directAutoMode
)

func modeOf(r *http.Request) directMode {
	if commandMode(r) {
		return directCommandMode
	}
	return directLinkMode
}

// DirectResult 是一次签发的结果：URL（HTTPS 链接）和 Command（加密 HTTP 下载的命令）最多有一个，都没有表示节点现在不能直连。
type DirectResult struct {
	URL     string
	Command *nodes.DirectHTTPCommand
}

// ErrNoDirect：节点现在不能直连下载（关了、没有公网地址、HTTPS 和加密 HTTP 都不能用、旧版本）。
var ErrNoDirect = i18n.New("该节点现在不能直连下载，请经面板下载")

// issueDirect 按 mode 签发，成功时记一次活动记录（record，参数里带链接编号，命令另带 direct_http）。
func issueDirect(ctx context.Context, conn *nodes.Conn, claims directdl.Claims, mode directMode, record func(extra map[string]any)) (DirectResult, error) {
	if mode != directCommandMode {
		if url := conn.DirectLink(claims); url != "" {
			record(nil)
			return DirectResult{URL: url}, nil
		}
		if mode == directLinkMode {
			return DirectResult{}, nil
		}
	}
	cmd, err := conn.DirectHTTP(ctx, claims)
	if err != nil || cmd == nil {
		return DirectResult{}, err
	}
	record(map[string]any{"direct_http": true})
	return DirectResult{Command: cmd}, nil
}

// volumeDirect 签发卷文件的直连下载。调用前已经检查过权限。
func (a *API) volumeDirect(ctx context.Context, u auth.User, ref *nagiv1.VolumeFileRef, mode directMode) (DirectResult, error) {
	nodeID, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return DirectResult{}, err
	}
	conn, err := a.Nodes.Get(nodeID)
	if err != nil {
		return DirectResult{}, nil
	}
	// 直连的链接和命令里有节点的 IP：没有「查看访问地址」、或者节点在范围外时一律经面板中转（A3、B8）。
	if !u.IsAdmin() {
		x, err := a.loadAccess(ctx, u)
		if err != nil {
			return DirectResult{}, err
		}
		if !u.Can(auth.PermAddresses) || !x.node(nodeID) {
			return DirectResult{}, nil
		}
	}
	id := directdl.NewID()
	claims := directdl.Claims{Kind: directdl.KindVolume, Project: ref.GetProjectId(), Container: ref.GetContainer(),
		Volume: ref.GetVolume(), Snapshot: ref.GetSnapshotId(), Path: ref.GetPath(), ID: id, User: u.Name}
	return issueDirect(ctx, conn, claims, mode, func(extra map[string]any) {
		params := map[string]any{"direct_link": id}
		maps.Copy(params, extra)
		a.recordCallWith(ctx, "/DownloadVolumeFile", ref, audit.ResultOK, "", 0, params)
	})
}

// nodeDirect 签发节点文件的直连下载。调用前已经检查过权限。
func (a *API) nodeDirect(ctx context.Context, u auth.User, nodeID, path string, mode directMode) (DirectResult, error) {
	conn, err := a.Nodes.Get(nodeID)
	if err != nil {
		return DirectResult{}, nil
	}
	id := directdl.NewID()
	claims := directdl.Claims{Kind: directdl.KindNode, Path: path, ID: id, User: u.Name}
	return issueDirect(ctx, conn, claims, mode, func(extra map[string]any) {
		params := map[string]any{"detail": path, "direct_link": id}
		maps.Copy(params, extra)
		b, _ := json.Marshal(params)
		a.Record(context.WithoutCancel(ctx), audit.Event{Action: "DownloadNodeFile", TargetType: "node", TargetID: nodeID,
			TargetName: a.nodeName(ctx, nodeID), Params: string(b)})
	})
}

// writeResult 按请求的方式回答浏览器：链接不能用时回 204（浏览器经面板下载），命令不能用时回 409。
func writeResult(w http.ResponseWriter, r *http.Request, res DirectResult, err error) {
	if err != nil {
		jsonFail(w, r, http.StatusBadGateway, err)
		return
	}
	if commandMode(r) {
		writeCommand(w, r, res.Command)
		return
	}
	writeDirect(w, res.URL)
}

// VolumeDirectHandler：GET /api/volume-file/direct?project=&container=&volume=&path=&snapshot=，参数同 /api/volume-file。
// 带 mode=command 时回答加密 HTTP 下载的命令（writeCommand）。
func (a *API) VolumeDirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Auth.RequestUser(r)
		if err != nil {
			jsonFail(w, r, http.StatusUnauthorized, i18n.New("请先登录"))
			return
		}
		q := r.URL.Query()
		if !u.Can(auth.PermFilesRead) {
			jsonFail(w, r, http.StatusForbidden, errNoPermission)
			return
		}
		if err := a.projectAllowed(r.Context(), u, q.Get("project")); err != nil {
			jsonFail(w, r, http.StatusNotFound, err)
			return
		}
		ctx := auth.WithRequest(auth.WithUser(r.Context(), u), r)
		ref := &nagiv1.VolumeFileRef{ProjectId: q.Get("project"), Container: q.Get("container"), Volume: q.Get("volume"), Path: q.Get("path"), SnapshotId: q.Get("snapshot")}
		if _, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer()); err != nil {
			jsonFail(w, r, http.StatusNotFound, err)
			return
		}
		res, err := a.volumeDirect(ctx, u, ref, modeOf(r))
		writeResult(w, r, res, err)
	})
}

// NodeDirectHandler：GET /api/node-file/direct?node=&path=，参数和权限同 /api/node-file 的下载（节点终端和文件，要先确认身份）。
// 带 mode=command 时回答加密 HTTP 下载的命令（writeCommand）。
func (a *API) NodeDirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Auth.RequestUser(r)
		if err != nil {
			jsonFail(w, r, http.StatusUnauthorized, i18n.New("请先登录"))
			return
		}
		q := r.URL.Query()
		nodeID, path := q.Get("node"), q.Get("path")
		ctx := auth.WithRequest(auth.WithUser(r.Context(), u), r)
		if err := a.nodeRootAllowed(ctx, u, nodeID); err != nil {
			a.recordNodeAccess(ctx, "DownloadNodeFile", nodeID, path+": 当前账号没有权限执行这个操作", true)
			jsonFail(w, r, http.StatusForbidden, errNoPermission)
			return
		}
		if !u.Elevated() {
			w.Header().Set(elevationHeader, "required")
			jsonFail(w, r, http.StatusForbidden, i18n.New("请先验证身份"))
			return
		}
		res, err := a.nodeDirect(ctx, u, nodeID, path, modeOf(r))
		writeResult(w, r, res, err)
	})
}

// VolumeDirectLink 是 MCP 的 get_download_link（见 docs/mcp.md#文件）：权限和 read_file 一样（checkAgentFiles：能操作容器、
// 访问范围内的项目，令牌要允许执行命令）。HTTPS 能用时给链接，否则给加密 HTTP 下载的命令，都不能用时返回 ErrNoDirect。
func (a *API) VolumeDirectLink(ctx context.Context, ref *nagiv1.VolumeFileRef) (DirectResult, error) {
	if err := a.checkAgentFiles(ctx, ref.GetProjectId(), false); err != nil {
		return DirectResult{}, err
	}
	res, err := a.volumeDirect(ctx, auth.CurrentUser(ctx), ref, directAutoMode)
	if err == nil && res.URL == "" && res.Command == nil {
		err = ErrNoDirect
	}
	return res, err
}

// NodeDirectLink 是 MCP 的 get_node_download_link：和 read_node_file 一样要「节点终端和文件」，令牌要勾「节点终端」
// （网页的会话要做过高权限确认）。规则同 VolumeDirectLink。
func (a *API) NodeDirectLink(ctx context.Context, nodeID, path string) (DirectResult, error) {
	u := auth.CurrentUser(ctx)
	if a.nodeRootAllowed(ctx, u, nodeID) != nil || !u.ElevatedFor(nagiv1connect.NodeServiceReadNodeFileProcedure) {
		a.recordNodeAccess(ctx, "DownloadNodeFile", nodeID, path+": 当前账号没有权限执行这个操作", true)
		return DirectResult{}, i18n.New("当前账号没有权限执行这个操作")
	}
	res, err := a.nodeDirect(ctx, u, nodeID, path, directAutoMode)
	if err == nil && res.URL == "" && res.Command == nil {
		err = ErrNoDirect
	}
	return res, err
}
