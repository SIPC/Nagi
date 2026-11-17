package oauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/safehttp"
)

// 客户端元数据只从公网地址取：本机（按域名、按 IP）和内网地址一律连不上。
func TestMetadataFetchPublicOnly(t *testing.T) {
	hit := false
	hs := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer hs.Close()
	s := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, id := range []string{
		strings.Replace(hs.URL, "127.0.0.1", "localhost", 1) + "/client.json",
		hs.URL + "/client.json",
		"https://10.255.255.1/client.json",
		"https://100.64.0.1/client.json",
		"https://169.254.169.254/client.json",
	} {
		_, err := s.fetchMetadata(context.Background(), id)
		if _, ok := safehttp.Blocked(err); !ok {
			t.Errorf("%s：想要因为地址不允许而失败，得到 %v", id, err)
		}
	}
	if hit {
		t.Fatal("请求到达了本机的服务")
	}
}
