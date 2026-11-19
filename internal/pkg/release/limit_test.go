package release

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// endlessServer 的 SHA256SUMS 是正常的，二进制却是 size 字节（size < 0 时是不带 Content-Length 的无穷数据）。
func endlessServer(t *testing.T, size int, withLength bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var bins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "SHA256SUMS":
			w.Write([]byte(sum("x") + "  nagi-linux-" + runtime.GOARCH + "\n"))
		case "nagi-linux-" + runtime.GOARCH:
			bins.Add(1)
			if withLength {
				w.Header().Set("Content-Length", strconv.Itoa(size))
			}
			chunk := make([]byte, 32<<10)
			for written := 0; size < 0 || written < size; {
				n := len(chunk)
				if size >= 0 {
					n = min(n, size-written)
				}
				if _, err := w.Write(chunk[:n]); err != nil {
					return // 客户端断开
				}
				written += n
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &bins
}

// v0.35.0#22：下载的新版本有大小上限。Content-Length 超过上限时不下载；没有 Content-Length 的无穷数据写到上限就停下，
// 不会写满磁盘以后才比较校验值。
func TestDownloadSizeLimit(t *testing.T) {
	prev := maxBinary
	maxBinary = 256 << 10
	defer func() { maxBinary = prev }()

	t.Run("Content-Length 超过上限", func(t *testing.T) {
		srv, _ := endlessServer(t, int(maxBinary)+1, true)
		dst := filepath.Join(t.TempDir(), "nagi.new")
		err := Download(context.Background(), srv.URL, dst)
		if err == nil || !strings.Contains(err.Error(), "不是正常的发布文件") {
			t.Fatalf("应该放弃：%v", err)
		}
		if info, err := os.Stat(dst); err == nil && info.Size() != 0 {
			t.Fatalf("不应该开始写：%d", info.Size())
		}
	})
	t.Run("没有 Content-Length 的无穷数据", func(t *testing.T) {
		srv, _ := endlessServer(t, -1, false)
		dst := filepath.Join(t.TempDir(), "nagi.new")
		err := Download(context.Background(), srv.URL, dst)
		if err == nil || !strings.Contains(err.Error(), "不是正常的发布文件") {
			t.Fatalf("应该写到上限就停下：%v", err)
		}
		if info, _ := os.Stat(dst); info.Size() > maxBinary+1 {
			t.Fatalf("写了 %d 字节，超过上限 %d", info.Size(), maxBinary)
		}
	})
	t.Run("上限以内照常核对", func(t *testing.T) {
		srv, _ := endlessServer(t, 64<<10, true)
		err := Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "nagi.new"))
		if err == nil || !strings.Contains(err.Error(), "校验不一致") {
			t.Fatalf("上限以内应该下载完再核对：%v", err)
		}
	})
}

// 写入时检查磁盘剩余空间，不够时停下，也不再换下一个地址（都写同一块盘）。
func TestDownloadLowDisk(t *testing.T) {
	prev := diskSpace
	defer func() { diskSpace = prev }()
	diskSpace = func(string) (int64, int64) { return 100 << 30, 1 << 30 }

	first, _ := endlessServer(t, 64<<10, true)
	second, bins := endlessServer(t, 64<<10, true)
	sources := []Source{{Base: first.URL, Mirror: true}, {Base: second.URL, Mirror: true}}
	_, err := fetchFirst(context.Background(), sources, filepath.Join(t.TempDir(), "x"), nil, slog.New(slog.DiscardHandler))
	if !isLowDisk(err) || !strings.Contains(err.Error(), "磁盘剩余空间不足") {
		t.Fatalf("应该报磁盘不足：%v", err)
	}
	if bins.Load() != 0 {
		t.Fatal("磁盘不够时不应该换下一个地址")
	}
}
