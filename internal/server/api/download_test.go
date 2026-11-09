package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadHandler(t *testing.T) {
	dir := t.TempDir()
	get := func(version, path string) int {
		a := &API{Panel: &PanelInfo{DataDir: dir, Version: version}}
		rec := httptest.NewRecorder()
		a.DownloadHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	// 本地编译的面板没有对应的发布文件；不认识的文件名一律 404。
	for _, c := range []struct{ version, path string }{
		{"dev", "/download/nagi-linux-amd64"},
		{"v0.2.0", "/download/nagi-linux-386"},
		{"v0.2.0", "/download/index.html"},
	} {
		if code := get(c.version, c.path); code != http.StatusNotFound {
			t.Errorf("%s %s：%d，应为 404", c.version, c.path, code)
		}
	}
	// 已经缓存的版本直接提供，不再访问 GitHub。
	cached := filepath.Join(dir, "release", "v0.2.0", "nagi-linux-arm64")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("bin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := get("v0.2.0", "/download/nagi-linux-arm64"); code != http.StatusOK {
		t.Errorf("已缓存的文件：%d，应为 200", code)
	}
}

// 同一个文件同时只下载一次；等待的请求可以自己放弃；失败的结果留一会儿，期间不再访问 GitHub。
func TestCachedReleaseSingleFlight(t *testing.T) {
	const name = "nagi-linux-amd64"
	bin := []byte("binary")
	sum := sha256.Sum256(bin)
	var hits atomic.Int32
	var failing atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		case "/" + name:
			<-release
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase, oldRetry := releaseBase, releaseRetry
	releaseBase = func(string) string { return srv.URL }
	t.Cleanup(func() { releaseBase, releaseRetry = oldBase, oldRetry })
	ctx := context.Background()

	dir := t.TempDir()
	// 等待的请求放弃时马上返回，下载照常进行。
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := cachedRelease(short, dir, "v0.2.0", name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("放弃等待：%v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() {
			_, err := cachedRelease(ctx, dir, "v0.2.0", name)
			errs <- err
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("应该只下载一次（SHA256SUMS 和文件各一次），实际 %d 次", n)
	}

	// 失败的结果在 releaseRetry 之内直接返回。
	failing.Store(true)
	hits.Store(0)
	dir = t.TempDir()
	for range 3 {
		if _, err := cachedRelease(ctx, dir, "v0.2.0", name); err == nil {
			t.Fatal("上游出错时应该失败")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("失败以后不应该马上重试，实际访问 %d 次", n)
	}
	releaseRetry = 0
	if _, err := cachedRelease(ctx, dir, "v0.2.0", name); err == nil || hits.Load() != 2 {
		t.Fatalf("过了 releaseRetry 应该重新下载：%v，访问 %d 次", err, hits.Load())
	}
}
