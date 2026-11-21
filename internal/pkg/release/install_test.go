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
		{"内容被改过", sum("other") + "  " + name + "\n", "校验不一致"},
		{"SHA256SUMS 里没有这个架构", sum(bin) + "  nagi-linux-riscv64\n", "SHA256SUMS 中没有"},
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
	noKnownProxies(t)
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
			got, _, err := Latest(context.Background(), "")
			if got != c.want || (err == nil) != (c.want != "") {
				t.Fatalf("得到 %q、%v，应为 %q", got, err, c.want)
			}
		})
	}
}

// 预发布渠道从发布列表里取版本号最大的；正式版渠道不看发布列表。正文里提到的版本链接（转义过的 HTML）不算。
func TestLatestInPre(t *testing.T) {
	noKnownProxies(t)
	oldFeed, oldLatest := feedURL, latestURL
	t.Cleanup(func() { feedURL, latestURL = oldFeed, oldLatest })
	entry := func(tag string) string {
		return `<entry><link rel="alternate" type="text/html" href="https://github.com/SIPC/Nagi/releases/tag/` + tag + `"/><title>` + tag + `</title></entry>`
	}
	feed := `<feed><link type="text/html" rel="alternate" href="https://github.com/SIPC/Nagi/releases"/>` +
		entry("v0.38.0-pre.2") + entry("v0.38.0-pre.10") + entry("v0.37.0") +
		`<content type="html">&lt;a href=&quot;https://github.com/SIPC/Nagi/releases/tag/v9.0.0&quot;&gt;</content></feed>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			w.Header().Set("Location", "https://github.com/SIPC/Nagi/releases/tag/v0.37.0")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Write([]byte(feed))
	}))
	defer srv.Close()
	feedURL, latestURL = srv.URL+"/feed", srv.URL+"/latest"
	if v, _, err := LatestIn(context.Background(), ChannelPre, ""); v != "v0.38.0-pre.10" || err != nil {
		t.Errorf("预发布渠道：%q、%v", v, err)
	}
	if v, _, err := LatestIn(context.Background(), ChannelStable, ""); v != "v0.37.0" || err != nil {
		t.Errorf("正式版渠道：%q、%v", v, err)
	}
	// 正式版发布以后，预发布渠道也拿到它（同号正式版比预发布版新）。
	feed = `<feed>` + entry("v0.38.0") + entry("v0.38.0-pre.10") + `</feed>`
	if v, _, _ := LatestIn(context.Background(), ChannelPre, ""); v != "v0.38.0" {
		t.Errorf("正式版发布后：%q", v)
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
		// 预发布号逐段比较，数字按数值：pre.2 < pre.10（按字符串会反过来）。
		{"v0.37.1-pre.2", "v0.37.1-pre.10", true},
		{"v0.37.1-pre.10", "v0.37.1-pre.2", false},
		{"v0.37.1-pre.2", "v0.37.1-pre.2", false},
		// 从预发布版可以升级到同号正式版，同号正式版不会被「升级」回预发布版。
		{"v0.37.1-pre.2", "v0.37.1", true},
		{"v0.37.1", "v0.37.1-pre.3", false},
		// 上一个正式版早于下一个版本的预发布版，预发布版早于更高修订号的正式版。
		{"v0.37.0", "v0.37.1-pre.1", true},
		{"v0.37.1-pre.9", "v0.37.2", true},
		// 数字段早于字母段，段数少的在前。
		{"v1.0.0-pre.1", "v1.0.0-pre.x", true},
		{"v1.0.0-pre", "v1.0.0-pre.1", true},
		// 构建信息不参与比较。
		{"v1.0.0+b1", "v1.0.0+b2", false},
	}
	for _, c := range cases {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%q, %q) = %v，应为 %v", c.a, c.b, got, c.want)
		}
	}
}

// 安装时用了 GitHub 加速代理：升级也经代理下载；同时有镜像地址时用镜像。
func TestBaseForProxy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("https://ghfast.top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BaseFor(dir, "v0.1.0"); got != "https://ghfast.top/"+DownloadBase("v0.1.0") {
		t.Errorf("经代理：%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("ftp://bad/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProxyFor(dir); got != "" {
		t.Errorf("不是 https 的代理应忽略：%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, downloadURLFile), []byte("https://mirror.example.com/nagi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BaseFor(dir, "v0.1.0"); got != "https://mirror.example.com/nagi" {
		t.Errorf("有镜像：%s", got)
	}
}

// 直接连不上 GitHub 时经代理查最新版本。
func TestLatestViaProxy(t *testing.T) {
	noKnownProxies(t)
	old := latestURL
	t.Cleanup(func() { latestURL = old })
	latestURL = "http://127.0.0.1:1/releases/latest" // 连不上
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+latestURL {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "https://github.com/SIPC/Nagi/releases/tag/v0.6.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	if got, via, err := Latest(context.Background(), srv.URL+"/"); got != "v0.6.0" || via != hostOf(srv.URL) || err != nil {
		t.Fatalf("得到 %q、%v", got, err)
	}
	if _, _, err := Latest(context.Background(), ""); err == nil {
		t.Fatal("没有代理时应报错")
	}
}
