package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SIPC/nagi/internal/agent"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 节点的上传名额满了返回 429（界面等一会儿重试），磁盘空间不足仍是 507（不重试）。
func TestChunkFailBusy(t *testing.T) {
	if uploadBusy != agent.UploadBusy {
		t.Fatalf("面板的 uploadBusy 要和节点的 agent.UploadBusy 一致：%q、%q", uploadBusy, agent.UploadBusy)
	}
	for _, c := range []struct {
		err  error
		want int
	}{
		{status.Error(codes.ResourceExhausted, uploadBusy), http.StatusTooManyRequests},
		{status.Error(codes.ResourceExhausted, "磁盘空间不足"), http.StatusInsufficientStorage},
	} {
		w := httptest.NewRecorder()
		chunkFail(w, httptest.NewRequest(http.MethodPut, "/api/volume-file", nil), http.StatusBadGateway, c.err)
		if w.Code != c.want {
			t.Errorf("%v：%d，应为 %d", c.err, w.Code, c.want)
		}
	}
}
