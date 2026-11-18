package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/byterange"
	"github.com/SIPC/nagi/internal/pkg/filehttp"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// fakeChunks 模拟节点发来的下载流。
type fakeChunks struct{ chunks []*nagiv1.VolumeFileChunk }

func (f *fakeChunks) Recv() (*nagiv1.VolumeFileChunk, error) {
	if len(f.chunks) == 0 {
		return nil, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return c, nil
}

// fakeNode 像节点那样发文件：supportsRange 为 false 时是不认识范围的旧版本，总是发整个文件。
func fakeNode(data []byte, supportsRange bool) func(context.Context, int64, int64) (chunkStream, error) {
	return func(_ context.Context, offset, length int64) (chunkStream, error) {
		size := int64(len(data))
		head := &nagiv1.VolumeFileChunk{Name: "a.bin", Size: size, ModifiedUnixNano: 1_700_000_000_000_000_000}
		body := data
		if supportsRange && (offset != 0 || length != 0) {
			head.Ranged = true
			part, ok, _ := byterange.Section(bytes.NewReader(data), offset, length, size)
			body = nil
			if ok {
				body, _ = io.ReadAll(part)
			}
		}
		chunks := []*nagiv1.VolumeFileChunk{head}
		for i := 0; i < len(body); i += 7 {
			chunks = append(chunks, &nagiv1.VolumeFileChunk{Data: body[i:min(i+7, len(body))]})
		}
		return &fakeChunks{chunks}, nil
	}
}

func TestFileDownloadRanges(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	etag := `"24-17979cfe362a0000"`
	cases := []struct {
		desc, rangeHeader, ifRange string
		supportsRange              bool
		code                       int
		body, contentRange         string
		recorded                   int
	}{
		{"整个文件", "", "", true, 200, string(data), "", 1},
		{"第一次分段请求记录（从中间开始也记）", "bytes=10-15", "", true, 206, "abcdef", "bytes 10-15/36", 1},
		{"之后的分段请求不重复记", "bytes=0-3", "", true, 206, "0123", "bytes 0-3/36", 0},
		{"最后几个字节", "bytes=-4", "", true, 206, "wxyz", "bytes 32-35/36", 0},
		{"超出文件", "bytes=100-", "", true, 416, "", "bytes */36", 0},
		{"If-Range 对得上", "bytes=30-", etag, true, 206, "uvwxyz", "bytes 30-35/36", 0},
		{"If-Range 对不上：发整个文件", "bytes=30-", `"other"`, true, 200, string(data), "", 1},
		{"旧版本节点忽略范围", "bytes=10-15", "", false, 200, string(data), "", 1},
		{"多段请求按整个文件", "bytes=0-1,5-6", "", true, 200, string(data), "", 1},
	}
	filehttp.ResetRanges()
	for _, c := range cases {
		recorded := 0
		req := httptest.NewRequest(http.MethodGet, "/api/volume-file", nil)
		if c.rangeHeader != "" {
			req.Header.Set("Range", c.rangeHeader)
		}
		if c.ifRange != "" {
			req.Header.Set("If-Range", c.ifRange)
		}
		rec := httptest.NewRecorder()
		fileDownload{
			open:   fakeNode(data, c.supportsRange),
			record: func(string) { recorded++ },
			fail:   func(code int, err error) { t.Fatalf("%s：fail(%d, %v)", c.desc, code, err) },
			key:    "u1|a.bin",
		}.serve(rec, req)
		if rec.Code != c.code || rec.Body.String() != c.body || rec.Header().Get("Content-Range") != c.contentRange || recorded != c.recorded {
			t.Errorf("%s：得到 %d %q Content-Range=%q 记录 %d 次", c.desc, rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"), recorded)
		}
		if c.code == 200 || c.code == 206 {
			if rec.Header().Get("Cache-Control") != "private, no-store" {
				t.Errorf("%s：下载不能被浏览器缓存", c.desc)
			}
			if rec.Header().Get("Accept-Ranges") != "bytes" || rec.Header().Get("ETag") != etag {
				t.Errorf("%s：Accept-Ranges=%q ETag=%q", c.desc, rec.Header().Get("Accept-Ranges"), rec.Header().Get("ETag"))
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(c.body)) {
				t.Errorf("%s：Content-Length=%s，应为 %d", c.desc, got, len(c.body))
			}
		}
	}
}

// 别人（或别的文件）的分段请求单独记。
func TestFileDownloadRangeRecordPerKey(t *testing.T) {
	data := []byte("0123456789")
	count := 0
	get := func(key string) {
		req := httptest.NewRequest(http.MethodGet, "/api/volume-file", nil)
		req.Header.Set("Range", "bytes=5-")
		fileDownload{open: fakeNode(data, true), record: func(string) { count++ }, fail: func(int, error) {}, key: key}.serve(httptest.NewRecorder(), req)
	}
	get("other-user|b.bin")
	get("other-user|b.bin")
	get("third-user|b.bin")
	if count != 2 {
		t.Fatalf("记了 %d 次，应为 2 次", count)
	}
}
