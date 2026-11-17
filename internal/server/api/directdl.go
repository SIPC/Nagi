package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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

func jsonFail(w http.ResponseWriter, r *http.Request, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": errorText(requestLang(r), err)})
}

// VolumeDirectHandler：GET /api/volume-file/direct?project=&container=&volume=&path=&snapshot=，参数同 /api/volume-file。
func (a *API) VolumeDirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Auth.RequestUser(r)
		if err != nil {
			jsonFail(w, r, http.StatusUnauthorized, i18n.New("请先登录"))
			return
		}
		if !auth.CanOperate(u.Role) {
			jsonFail(w, r, http.StatusForbidden, i18n.New("当前账号没有权限执行这个操作"))
			return
		}
		q := r.URL.Query()
		if err := a.projectAllowed(r.Context(), u, q.Get("project")); err != nil {
			jsonFail(w, r, http.StatusNotFound, err)
			return
		}
		ctx := auth.WithRequest(auth.WithUser(r.Context(), u), r)
		ref := &nagiv1.VolumeFileRef{ProjectId: q.Get("project"), Container: q.Get("container"), Volume: q.Get("volume"), Path: q.Get("path"), SnapshotId: q.Get("snapshot")}
		nodeID, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer())
		if err != nil {
			jsonFail(w, r, http.StatusNotFound, err)
			return
		}
		conn, err := a.Nodes.Get(nodeID)
		if err != nil {
			writeDirect(w, "")
			return
		}
		id := directdl.NewID()
		url := conn.DirectLink(directdl.Claims{Kind: directdl.KindVolume, Project: ref.GetProjectId(), Container: ref.GetContainer(),
			Volume: ref.GetVolume(), Snapshot: ref.GetSnapshotId(), Path: ref.GetPath(), ID: id, User: u.Name})
		if url != "" {
			a.recordCallWith(ctx, "/DownloadVolumeFile", ref, audit.ResultOK, "", 0, map[string]any{"direct_link": id})
		}
		writeDirect(w, url)
	})
}

// NodeDirectHandler：GET /api/node-file/direct?node=&path=，参数和权限同 /api/node-file 的下载（管理员，要先确认身份）。
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
		if u.Role != auth.RoleAdmin {
			a.recordNodeAccess(ctx, "DownloadNodeFile", nodeID, path+": 当前账号没有权限执行这个操作", true)
			jsonFail(w, r, http.StatusForbidden, i18n.New("当前账号没有权限执行这个操作"))
			return
		}
		if !u.Elevated() {
			w.Header().Set(elevationHeader, "required")
			jsonFail(w, r, http.StatusForbidden, i18n.New("请先验证身份"))
			return
		}
		conn, err := a.Nodes.Get(nodeID)
		if err != nil {
			writeDirect(w, "")
			return
		}
		id := directdl.NewID()
		url := conn.DirectLink(directdl.Claims{Kind: directdl.KindNode, Path: path, ID: id, User: u.Name})
		if url != "" {
			b, _ := json.Marshal(map[string]string{"detail": path, "direct_link": id})
			a.Record(context.WithoutCancel(ctx), audit.Event{Action: "DownloadNodeFile", TargetType: "node", TargetID: nodeID,
				TargetName: a.nodeName(ctx, nodeID), Params: string(b)})
		}
		writeDirect(w, url)
	})
}
