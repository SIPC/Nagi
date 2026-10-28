package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// releaseServer 模拟 GitHub Release（或镜像）：提供 nagi-linux-<架构> 和 SHA256SUMS。
func releaseServer(t *testing.T, binary, sums string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "nagi-linux-" + runtime.GOARCH:
			w.Write([]byte(binary))
		case "SHA256SUMS":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestDownload(t *testing.T) {
	name := "nagi-linux-" + runtime.GOARCH
	bin := "#!/bin/sh\necho v0.1.0\n"
	cases := []struct {
		desc, sums, wantErr string
	}{
		{"校验通过", sum(bin) + "  " + name + "\n", ""},
		{"二进制模式的条目", sum(bin) + " *" + name + "\n", ""},
		{"内容被改过", sum("other") + "  " + name + "\n", "校验不通过"},
		{"SHA256SUMS 里没有这个架构", sum(bin) + "  nagi-linux-riscv64\n", "SHA256SUMS 里没有"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			srv := releaseServer(t, bin, c.sums)
			dst := filepath.Join(t.TempDir(), "nagi.new")
			err := download(context.Background(), srv.URL, dst)
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if b, _ := os.ReadFile(dst); string(b) != bin {
					t.Fatalf("下载的内容不对：%q", b)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("应报错 %q，实际：%v", c.wantErr, err)
			}
		})
	}
	t.Run("下载地址不存在", func(t *testing.T) {
		srv := releaseServer(t, bin, "")
		if err := download(context.Background(), srv.URL+"/missing", filepath.Join(t.TempDir(), "x")); err == nil {
			t.Fatal("应报错")
		}
	})
}
