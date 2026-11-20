package oauth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/safehttp"
)

func TestMetadataNegativeCacheDropsRemoteErrors(t *testing.T) {
	remote := "remote-secret-" + strings.Repeat("x", 128<<10)
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint("malformed=", malformed), func(t *testing.T) {
			hs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				if malformed {
					fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\n%s\r\n\r\n", remote)
				} else {
					fmt.Fprintf(conn, "HTTP/1.1 503 %s\r\nContent-Length: 0\r\n\r\n", remote)
				}
			}))
			defer hs.Close()
			s := New(nil, nil)
			s.fetch = hs.Client() // 本地 TLS 测试，不改变生产环境的公网地址限制。
			id := hs.URL + "/client.json"
			for range 2 {
				_, err := s.client(context.Background(), id)
				if err == nil || len(err.Error()) > 256 || strings.Contains(err.Error(), "remote-secret") {
					t.Fatalf("remote error exposed: %.256v", err)
				}
			}
			cached := s.docs[id].err
			if cached == nil || len(cached.Error()) > 256 || strings.Contains(cached.Error(), "remote-secret") {
				t.Fatalf("remote error retained: %.256v", cached)
			}
		})
	}
}

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
