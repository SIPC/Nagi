package api

import (
	"encoding/json"
	"net/http"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 直连下载（见 docs/agent.md#直连下载）：权限和经面板下载一样在这里检查，通过后签一个 15 分钟有效的链接，
// 浏览器拿着它直接从节点下载。节点不能直连时回 204，浏览器照旧经面板下载。签发链接时就记进活动记录：
// 之后的下载不经面板，面板看不到。

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
		url := conn.DirectLink(directdl.Claims{Kind: directdl.KindVolume, Project: ref.GetProjectId(), Container: ref.GetContainer(),
			Volume: ref.GetVolume(), Snapshot: ref.GetSnapshotId(), Path: ref.GetPath()})
		if url != "" {
			a.recordOperation(ctx, "/DownloadVolumeFile", ref, "")
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
		url := conn.DirectLink(directdl.Claims{Kind: directdl.KindNode, Path: path})
		if url != "" {
			a.recordNodeAccess(ctx, "DownloadNodeFile", nodeID, path, false)
		}
		writeDirect(w, url)
	})
}
