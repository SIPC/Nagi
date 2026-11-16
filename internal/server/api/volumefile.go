package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// maxVolumeUpload 是上传到卷的单个文件的上限。
const maxVolumeUpload = 8 << 30

// VolumeFileHandler 下载和上传卷里的文件：GET、POST /api/volume-file?project=&container=&volume=&path=&snapshot=。
// 不走 Connect，因为浏览器要直接下载、流式上传。要求已登录且能操作容器；上传还要带 X-Nagi-Upload 请求头，跨站页面发不出这样的请求。
// 内容经 mesh 和节点传输，大文件不占用控制连接（见 docs/control-plane.md）。mesh 直连不通时，WireGuard 流量自动经面板中转。
func (a *API) VolumeFileHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		fail := func(code int, err error) {
			msg := errorText(lang, err)
			if isStatus(err) {
				switch status.Code(err) {
				case codes.Unavailable, codes.DeadlineExceeded:
					msg = i18n.Localize(lang, i18n.New("无法连接容器所在的节点，请稍后重试"))
				case codes.Unimplemented:
					msg = i18n.Localize(lang, i18n.New("节点版本过低，升级后才能管理文件"))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		}
		u, err := a.Auth.SessionUser(r.Context(), auth.SessionToken(r.Header))
		if err != nil {
			fail(http.StatusUnauthorized, i18n.New("请先登录"))
			return
		}
		if !auth.CanOperate(u.Role) {
			fail(http.StatusForbidden, i18n.New("当前账号没有权限执行这个操作"))
			return
		}
		if err := a.projectAllowed(r.Context(), u, r.URL.Query().Get("project")); err != nil {
			fail(http.StatusNotFound, err)
			return
		}
		q := r.URL.Query()
		if r.Method != http.MethodGet {
			if err := a.checkPrivilegedWrite(r.Context(), u.Role, q.Get("project"), q.Get("container")); err != nil {
				fail(http.StatusForbidden, err)
				return
			}
		}
		ref := &nagiv1.VolumeFileRef{
			ProjectId: q.Get("project"), Container: q.Get("container"), Volume: q.Get("volume"), Path: q.Get("path"), SnapshotId: q.Get("snapshot"),
		}
		ctx := auth.WithUser(r.Context(), u)
		m := a.Nodes.Mesh()
		if m == nil {
			fail(http.StatusServiceUnavailable, i18n.New("面板的节点网络没有启动，无法传输文件"))
			return
		}
		nodeID, err := a.Projects.Locate(ctx, ref.GetProjectId(), ref.GetContainer())
		if err != nil {
			fail(http.StatusNotFound, err)
			return
		}
		key, err := a.Nodes.NodeMeshKey(nodeID)
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
		cc, err := mesh.Client(m, key)
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
		defer cc.Close()
		client := nagiv1.NewMeshServiceClient(cc)

		if r.Method == http.MethodGet {
			// check=1 只确认能下载，界面先问一次，出错时留在页面上提示。真正下载时才记录。
			fileDownload{
				open: func(ctx context.Context, offset, length int64) (chunkStream, error) {
					part := proto.CloneOf(ref)
					part.Offset, part.Length = offset, length
					return client.VolumeDownload(ctx, part)
				},
				record: func(failure string) { a.recordOperation(ctx, "/DownloadVolumeFile", ref, failure) },
				fail:   fail,
				check:  q.Get("check") == "1",
				key:    fmt.Sprintf("%d|%s|%s|%s|%s|%s", u.ID, ref.GetProjectId(), ref.GetContainer(), ref.GetVolume(), ref.GetSnapshotId(), ref.GetPath()),
			}.serve(w, r)
			return
		}

		if r.Header.Get("X-Nagi-Upload") != "1" {
			fail(http.StatusBadRequest, i18n.New("缺少 X-Nagi-Upload 请求头"))
			return
		}
		// 浏览器也会先拦下；这里先看长度，免得白传一遍才报错。
		if r.ContentLength > maxVolumeUpload {
			a.recordOperation(ctx, "/UploadVolumeFile", ref, "file too large")
			fail(http.StatusRequestEntityTooLarge, i18n.New("单个文件不能超过 8 GB"))
			return
		}
		stream, err := client.VolumeUpload(ctx)
		if err != nil {
			a.recordOperation(ctx, "/UploadVolumeFile", ref, activityFailure(err))
			fail(http.StatusBadGateway, err)
			return
		}
		if err := stream.Send(&nagiv1.VolumeUploadChunk{Ref: ref}); err != nil {
			a.recordOperation(ctx, "/UploadVolumeFile", ref, activityFailure(err))
			fail(http.StatusBadGateway, err)
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxVolumeUpload)
		buf := make([]byte, 256<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if err := stream.Send(&nagiv1.VolumeUploadChunk{Data: buf[:n]}); err != nil {
					break // 节点提前结束（例如拒绝），真正的原因在 CloseAndRecv 里。
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				var tooBig *http.MaxBytesError
				if errors.As(rerr, &tooBig) {
					a.recordOperation(ctx, "/UploadVolumeFile", ref, "file too large")
					fail(http.StatusRequestEntityTooLarge, i18n.New("单个文件不能超过 8 GB"))
					return
				}
				a.recordOperation(ctx, "/UploadVolumeFile", ref, activityFailure(rerr))
				fail(http.StatusBadRequest, rerr)
				return
			}
		}
		res, err := stream.CloseAndRecv()
		if err != nil {
			a.recordOperation(ctx, "/UploadVolumeFile", ref, activityFailure(err))
			fail(http.StatusBadRequest, err)
			return
		}
		a.recordOperation(ctx, "/UploadVolumeFile", ref, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"size": res.GetSize()})
	})
}
