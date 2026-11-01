package release

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
			err := Download(context.Background(), srv.URL, dst)
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
		if err := Download(context.Background(), srv.URL+"/missing", filepath.Join(t.TempDir(), "x")); err == nil {
			t.Fatal("应报错")
		}
	})
}

func TestBaseFor(t *testing.T) {
	dir := t.TempDir()
	if got := BaseFor(dir, "v0.1.0"); got != DownloadBase("v0.1.0") {
		t.Errorf("没有镜像：%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, downloadURLFile), []byte(" https://mirror.example.com/nagi/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BaseFor(dir, "v0.1.0"); got != "https://mirror.example.com/nagi" {
		t.Errorf("有镜像：%s", got)
	}
}

func TestLatest(t *testing.T) {
	cases := []struct {
		desc, location string
		status         int
		want           string
	}{
		{"重定向到版本页", "https://github.com/SIPC/Nagi/releases/tag/v0.5.0", http.StatusFound, "v0.5.0"},
		{"还没有发布过", "", http.StatusNotFound, ""},
		{"重定向到别的地方", "https://github.com/login", http.StatusFound, ""},
	}
	old := latestURL
	t.Cleanup(func() { latestURL = old })
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.location != "" {
					w.Header().Set("Location", c.location)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			latestURL = srv.URL
			got, err := Latest(context.Background())
			if got != c.want || (err == nil) != (c.want != "") {
				t.Fatalf("得到 %q、%v，应为 %q", got, err, c.want)
			}
		})
	}
}

func TestOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.4.2", "v0.5.0", true},
		{"v0.9.0", "v0.10.0", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.5.1", "v0.5.0", false},
		{"v0.5.0-rc.1", "v0.5.0", true},
		{"v0.5.0", "v0.5.0-rc.1", false},
		{"dev", "v0.5.0", false},
		{"v0.5.0", "dev", false},
	}
	for _, c := range cases {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%q, %q) = %v，应为 %v", c.a, c.b, got, c.want)
		}
	}
}
