package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 节点的上传名额满了返回 429（界面等一会儿重试），磁盘空间不足仍是 507（不重试）。
func TestChunkFailBusy(t *testing.T) {
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
