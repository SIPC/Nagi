package release

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noKnownProxies 让测试不去连真正的加速代理。
func noKnownProxies(t *testing.T) {
	t.Helper()
	old := knownProxies
	knownProxies = nil
	t.Cleanup(func() { knownProxies = old })
}

func TestSources(t *testing.T) {
	old := knownProxies
	t.Cleanup(func() { knownProxies = old })
	knownProxies = []string{"https://a.example/", "https://b.example/"}
	direct := DownloadBase("v0.1.0")
	bases := func(dir string) []string {
		var out []string
		for _, s := range Sources(dir, "v0.1.0") {
			out = append(out, s.Base)
		}
		return out
	}

	dir := t.TempDir()
	if got, want := bases(dir), []string{direct, "https://a.example/" + direct, "https://b.example/" + direct}; !slices.Equal(got, want) {
		t.Errorf("没有记下代理：%v", got)
	}
	// 记着的代理排在最前，不重复。
	os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("https://b.example\n"), 0o644)
	if got, want := bases(dir), []string{"https://b.example/" + direct, direct, "https://a.example/" + direct}; !slices.Equal(got, want) {
		t.Errorf("记着代理：%v", got)
	}
	os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("https://c.example/\n"), 0o644)
	if got, want := bases(dir), []string{"https://c.example/" + direct, direct, "https://a.example/" + direct, "https://b.example/" + direct}; !slices.Equal(got, want) {
		t.Errorf("记着列表外的代理：%v", got)
	}
	// 指定了镜像时只用镜像。
	os.WriteFile(filepath.Join(dir, downloadURLFile), []byte("https://mirror.example.com/nagi/\n"), 0o644)
	if got := Sources(dir, "v0.1.0"); len(got) != 1 || got[0].Base != "https://mirror.example.com/nagi" || !got[0].Mirror {
		t.Errorf("有镜像：%+v", got)
	}

	names := map[string]Source{
		"github.com":         {Base: direct},
		"ghfast.top":         {Base: "https://ghfast.top/" + direct, Proxy: "https://ghfast.top/"},
		"mirror.example.com": {Base: "https://mirror.example.com/nagi", Mirror: true},
	}
	for want, s := range names {
		if got := s.Name(); got != want {
			t.Errorf("%+v 的名字是 %q，应为 %q", s, got, want)
		}
	}
}

func TestGitHubProxiesFormat(t *testing.T) {
	for _, p := range strings.Fields(GitHubProxies) {
		if !strings.HasPrefix(p, "https://") || !strings.HasSuffix(p, "/") {
			t.Errorf("代理 %q 须以 https:// 开头、以 / 结尾（ProxyFor 只认这样的）", p)
		}
	}
}

// fakeRelease 模拟 GitHub 或一个加速代理。mode：
//   - ok：正常提供文件和 SHA256SUMS；
//   - down：都返回 500；
//   - binDown：SHA256SUMS 正常，文件返回 500；
//   - hang：发了响应头就不再发数据；
//   - trickle：文件一直有数据，但很慢（每秒约 4 KB，1 MB 要四分多钟）；
//   - corrupt：文件被改过，SHA256SUMS 还是原来的；
//   - evil：换了一份文件，SHA256SUMS 也换成和它对得上的。
type fakeRelease struct {
	srv  *httptest.Server
	mode string
	hits atomic.Int32
}

const goodBin = "#!/bin/sh\necho good\n"
const evilBin = "#!/bin/sh\necho evil\n"

func newFake(t *testing.T, mode string) *fakeRelease {
	t.Helper()
	f := &fakeRelease{mode: mode}
	name := "nagi-linux-" + runtime.GOARCH
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		isSums := strings.HasSuffix(r.URL.Path, "/SHA256SUMS")
		isBin := strings.HasSuffix(r.URL.Path, "/"+name)
		if !isSums && !isBin {
			http.NotFound(w, r)
			return
		}
		switch f.mode {
		case "down":
			w.WriteHeader(http.StatusInternalServerError)
		case "binDown":
			if isBin {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(sum(goodBin) + "  " + name + "\n"))
		case "hang":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "trickle":
			if isSums {
				w.Write([]byte(sum(goodBin) + "  " + name + "\n"))
				return
			}
			w.Header().Set("Content-Length", "1048576")
			w.WriteHeader(http.StatusOK)
			for {
				w.Write(make([]byte, 200))
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		case "corrupt":
			if isSums {
				w.Write([]byte(sum(goodBin) + "  " + name + "\n"))
			} else {
				w.Write([]byte(evilBin))
			}
		case "evil":
			if isSums {
				w.Write([]byte(sum(evilBin) + "  " + name + "\n"))
			} else {
				w.Write([]byte(evilBin))
			}
		default:
			if isSums {
				w.Write([]byte(sum(goodBin) + "  " + name + "\n"))
			} else {
				w.Write([]byte(goodBin))
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setup 用 httptest 的证书下载（所有 TLS 测试服务器共用一张证书），缩短测速的时间。
func setup(t *testing.T) {
	t.Helper()
	noKnownProxies(t)
	probe := httptest.NewTLSServer(http.NotFoundHandler())
	oldT, oldTime := transport, lowSpeedTime
	transport, lowSpeedTime = probe.Client().Transport, 300*time.Millisecond
	probe.Close()
	t.Cleanup(func() { transport, lowSpeedTime = oldT, oldTime })
}

// sourcesOf 按顺序排出地址：第一个是「GitHub」，其余是经它的加速代理。
func sourcesOf(direct *fakeRelease, proxies ...*fakeRelease) []Source {
	base := direct.srv.URL + "/SIPC/Nagi/releases/download/v0.1.0"
	out := []Source{{Base: base}}
	for _, p := range proxies {
		out = append(out, Source{Base: p.srv.URL + "/" + base, Proxy: p.srv.URL + "/"})
	}
	return out
}

func readProxyFile(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, githubProxyFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestFetchFirst(t *testing.T) {
	setup(t)
	quiet := slog.New(slog.DiscardHandler)
	run := func(t *testing.T, sources []Source) (Source, string, error) {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "nagi.new")
		src, err := fetchFirst(context.Background(), sources, dst, nil, quiet)
		b, _ := os.ReadFile(dst)
		return src, string(b), err
	}

	t.Run("记着的代理和 GitHub 都不行，换下一个代理并记下它", func(t *testing.T) {
		dir := t.TempDir()
		recorded, direct, p2 := newFake(t, "down"), newFake(t, "down"), newFake(t, "ok")
		s := sourcesOf(direct, recorded, p2)
		s = []Source{s[1], s[0], s[2]} // 和 Sources 一样，记着的代理在最前
		os.WriteFile(filepath.Join(dir, githubProxyFile), []byte(s[0].Proxy+"\n"), 0o644)
		src, got, err := run(t, s)
		if err != nil || src.Proxy != p2.srv.URL+"/" || got != goodBin {
			t.Fatalf("得到 %+v、%q、%v", src, got, err)
		}
		if recorded.hits.Load() == 0 || direct.hits.Load() == 0 {
			t.Fatal("应按顺序先试记着的代理和 GitHub")
		}
		if err := remember(dir, src); err != nil {
			t.Fatal(err)
		}
		if got := readProxyFile(dir); got != p2.srv.URL+"/" {
			t.Fatalf("应记下用上的代理，实际 %q", got)
		}
	})

	t.Run("GitHub 能用时不试代理，并删掉记着的代理", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("https://ghfast.top/\n"), 0o644)
		direct, p := newFake(t, "ok"), newFake(t, "ok")
		src, got, err := run(t, sourcesOf(direct, p))
		if err != nil || !src.direct() || got != goodBin {
			t.Fatalf("得到 %+v、%q、%v", src, got, err)
		}
		if p.hits.Load() != 0 {
			t.Fatal("GitHub 成功后不应再试代理")
		}
		if err := remember(dir, src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, githubProxyFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("直接下载成功后应删掉 github-proxy")
		}
	})

	t.Run("校验不一致的地址跳过", func(t *testing.T) {
		direct, bad, good := newFake(t, "down"), newFake(t, "corrupt"), newFake(t, "ok")
		src, got, err := run(t, sourcesOf(direct, bad, good))
		if err != nil || src.Proxy != good.srv.URL+"/" || got != goodBin {
			t.Fatalf("得到 %+v、%q、%v", src, got, err)
		}
	})

	t.Run("拿到了 GitHub 的 SHA256SUMS 时代理不能换文件", func(t *testing.T) {
		direct, evil, good := newFake(t, "binDown"), newFake(t, "evil"), newFake(t, "ok")
		src, got, err := run(t, sourcesOf(direct, evil, good))
		if err != nil || src.Proxy != good.srv.URL+"/" || got != goodBin {
			t.Fatalf("得到 %+v、%q、%v", src, got, err)
		}
		if _, got, err := run(t, sourcesOf(direct, evil)); err == nil || got == evilBin {
			t.Fatalf("只有换了文件的代理时应失败，实际 %q、%v", got, err)
		}
		// 单独看代理那一次：SHA256SUMS 和 GitHub 上的对不上，文件都不下载。
		pinned := sum(goodBin)
		s := sourcesOf(direct, evil)[1]
		before := evil.hits.Load()
		if _, err := download(context.Background(), s.Base, filepath.Join(t.TempDir(), "x"), pinned); err == nil || !strings.Contains(err.Error(), "和 GitHub 上的不一致") {
			t.Fatalf("应报 SHA256SUMS 不一致，实际 %v", err)
		}
		if n := evil.hits.Load() - before; n != 1 {
			t.Fatalf("SHA256SUMS 对不上时不应再下载文件，请求了 %d 次", n)
		}
	})

	t.Run("太慢的地址放弃", func(t *testing.T) {
		direct, good := newFake(t, "hang"), newFake(t, "ok")
		start := time.Now()
		src, got, err := run(t, sourcesOf(direct, good))
		if err != nil || src.Proxy != good.srv.URL+"/" || got != goodBin {
			t.Fatalf("得到 %+v、%q、%v", src, got, err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("放弃得太晚：%v", time.Since(start))
		}
	})

	t.Run("按目前的速度在时限内下载不完就换下一个", func(t *testing.T) {
		direct, good := newFake(t, "trickle"), newFake(t, "ok")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		src, err := fetchFirst(ctx, sourcesOf(direct, good), filepath.Join(t.TempDir(), "x"), nil, quiet)
		if err != nil || src.Proxy != good.srv.URL+"/" {
			t.Fatalf("得到 %+v、%v", src, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("放弃得太晚：%v", time.Since(start))
		}
	})

	t.Run("都不行", func(t *testing.T) {
		_, _, err := run(t, sourcesOf(newFake(t, "down"), newFake(t, "down"), newFake(t, "corrupt")))
		if err == nil || !strings.Contains(err.Error(), "已尝试 3 个下载地址") {
			t.Fatalf("应报错，实际 %v", err)
		}
		_, _, err = run(t, sourcesOf(newFake(t, "down")))
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("只有一个地址时返回它的错误，实际 %v", err)
		}
	})

	t.Run("文件不能用时不再换地址", func(t *testing.T) {
		direct, p := newFake(t, "ok"), newFake(t, "ok")
		dst := filepath.Join(t.TempDir(), "nagi.new")
		_, err := fetchFirst(context.Background(), sourcesOf(direct, p), dst, func(Source) error {
			return unusable{errors.New("bad")}
		}, quiet)
		if !IsUnusable(err) || p.hits.Load() != 0 {
			t.Fatalf("得到 %v，代理请求 %d 次", err, p.hits.Load())
		}
	})

	t.Run("取消后不再换地址", func(t *testing.T) {
		direct, p := newFake(t, "hang"), newFake(t, "ok")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := fetchFirst(ctx, sourcesOf(direct, p), filepath.Join(t.TempDir(), "x"), nil, quiet)
		if !errors.Is(err, context.Canceled) || p.hits.Load() != 0 {
			t.Fatalf("得到 %v，代理请求 %d 次", err, p.hits.Load())
		}
	})
}

// 记着的是镜像时不动 github-proxy。
func TestRememberMirror(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, githubProxyFile), []byte("https://ghfast.top/\n"), 0o644)
	if err := remember(dir, Source{Base: "https://mirror.example.com", Mirror: true}); err != nil {
		t.Fatal(err)
	}
	if got := readProxyFile(dir); got != "https://ghfast.top/" {
		t.Fatalf("github-proxy 被改成了 %q", got)
	}
}

// 直接连不上 GitHub、也没有记着代理时，依次经内置的加速代理查最新版本。
func TestLatestViaKnownProxies(t *testing.T) {
	old, oldURL := knownProxies, latestURL
	t.Cleanup(func() { knownProxies, latestURL = old, oldURL })
	latestURL = "http://127.0.0.1:1/releases/latest"
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/SIPC/Nagi/releases/tag/v0.7.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer good.Close()
	knownProxies = []string{"http://127.0.0.1:1/", good.URL + "/"}
	if got, err := Latest(context.Background(), ""); got != "v0.7.0" || err != nil {
		t.Fatalf("得到 %q、%v", got, err)
	}
}
