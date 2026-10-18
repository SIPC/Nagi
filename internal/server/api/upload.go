package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/grpc/status"
)

// UploadHandler 接收浏览器上传的项目文件压缩包，经 mesh 转给节点：POST /api/upload?node=&project=，
// 请求体就是压缩包。节点解压后暂存，随后用 DeployNodeProject(use_upload) 部署。
// 要求带 X-Nagi-Upload 请求头，跨站页面发不出这样的请求。
func (a *API) UploadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(code int, err error) {
			if st, ok := status.FromError(err); ok {
				err = errors.New(st.Message())
			}
			reply(code, map[string]string{"message": err.Error()})
		}
		if _, err := a.Auth.Authenticate(r.Context(), auth.SessionToken(r.Header)); err != nil {
			fail(http.StatusUnauthorized, errors.New("未登录"))
			return
		}
		if r.Header.Get("X-Nagi-Upload") != "1" {
			fail(http.StatusBadRequest, errors.New("缺少 X-Nagi-Upload 请求头"))
			return
		}
		q := r.URL.Query()
		m := a.Nodes.Mesh()
		if m == nil {
			fail(http.StatusServiceUnavailable, errors.New("面板的 mesh 没有启动，无法上传"))
			return
		}
		key, err := a.Nodes.NodeMeshKey(q.Get("node"))
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
		stream, err := nagiv1.NewMeshServiceClient(cc).Upload(r.Context())
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
		if err := stream.Send(&nagiv1.UploadChunk{ProjectId: q.Get("project"), Container: q.Get("container")}); err != nil {
			fail(http.StatusBadGateway, err)
			return
		}
		buf := make([]byte, 256<<10)
		for {
			n, rerr := r.Body.Read(buf)
			if n > 0 {
				if err := stream.Send(&nagiv1.UploadChunk{Data: buf[:n]}); err != nil {
					// 节点提前结束（例如拒绝），真正的原因在 CloseAndRecv 里。
					break
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				fail(http.StatusBadRequest, rerr)
				return
			}
		}
		resp, err := stream.CloseAndRecv()
		if err != nil {
			fail(http.StatusBadGateway, err)
			return
		}
		reply(http.StatusOK, map[string]any{"bytes": resp.GetBytes(), "files": resp.GetFiles(), "stripped": resp.GetStripped()})
	})
}
