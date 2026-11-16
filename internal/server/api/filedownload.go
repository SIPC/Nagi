package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/byterange"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// chunkStream 是节点发来的下载流（卷文件、节点文件）：第一条带文件名、大小，之后是内容。
type chunkStream interface {
	Recv() (*nagiv1.VolumeFileChunk, error)
}

// fileDownload 是一次从节点下载文件的请求（卷文件、节点文件）。
type fileDownload struct {
	// open 让节点发文件；offset、length 不为 0 时只要一段（见 byterange）。
	open func(ctx context.Context, offset, length int64) (chunkStream, error)
	// record 记进活动记录，参数是失败原因（成功时为空）。
	record func(failure string)
	fail   func(code int, err error)
	// check：只确认能下载（界面先问一次），不发内容、不记录。
	check bool
	// key 标识谁在下载哪个文件（用户、项目或节点、路径），分段请求按它去重。
	key string
}

// rangeRecordTTL：同一个人下载同一个文件（同一个 ETag）的分段请求，这么久内只记一次。
// 多线程下载工具会发很多次分段请求，一一记下会把活动记录刷满；一次都不记又能绕过审计（取从第二个字节开始的部分）。
const rangeRecordTTL = 10 * time.Minute

var recentRanges = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// firstRange 判断这是不是 key 在 rangeRecordTTL 内的第一次分段请求，并记下这次。
func firstRange(key string) bool {
	recentRanges.Lock()
	defer recentRanges.Unlock()
	now := time.Now()
	if len(recentRanges.m) > 10000 {
		for k, at := range recentRanges.m {
			if now.Sub(at) > rangeRecordTTL {
				delete(recentRanges.m, k)
			}
		}
	}
	if at, ok := recentRanges.m[key]; ok && now.Sub(at) < rangeRecordTTL {
		return false
	}
	recentRanges.m[key] = now
	return true
}

// serve 把节点发来的文件写给浏览器，一律作为附件下载。普通文件支持单段的 Range 请求（断点续传、多线程下载工具），
// 用 ETag 核对续传期间文件没有变；节点版本旧不支持时照常发整个文件。目录打包成 tar.gz，不支持 Range。
// 整个文件的下载每次都记进活动记录；分段请求同一个人、同一个文件在 rangeRecordTTL 内只记一次。
func (d fileDownload) serve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	offset, length, ranged := int64(0), int64(0), false
	if !d.check {
		offset, length, ranged = byterange.Parse(r.Header.Get("Range"))
	}
	stream, head, err := d.start(ctx, offset, length)
	if err != nil {
		return
	}
	if d.check {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	etag, modified := "", time.Time{}
	if !head.GetArchive() && head.GetModifiedUnixNano() != 0 {
		etag = fmt.Sprintf(`"%x-%x"`, head.GetSize(), head.GetModifiedUnixNano())
		modified = time.Unix(0, head.GetModifiedUnixNano()).UTC()
	}
	// If-Range 和现在的文件对不上（文件变了）：续传接不上，改发整个文件。
	if ranged && head.GetRanged() && !ifRangeMatches(r.Header.Get("If-Range"), etag, modified) {
		cancel()
		ctx, cancel = context.WithCancel(r.Context())
		defer cancel()
		ranged = false
		if stream, head, err = d.start(ctx, 0, 0); err != nil {
			return
		}
	}
	h := w.Header()
	// 禁止猜类型：文件内容来自容器或节点，不能在面板的域名下当网页打开。
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	// 带了 ETag、Last-Modified 后浏览器会自行缓存，再下载同一个地址可能拿到旧的内容。不缓存不影响续传：
	// 续传靠的是 If-Range。
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": head.GetName()}))
	code := http.StatusOK
	if !head.GetArchive() {
		h.Set("Accept-Ranges", "bytes")
		if etag != "" {
			h.Set("ETag", etag)
			h.Set("Last-Modified", modified.Format(http.TimeFormat))
		}
		h.Set("Content-Length", strconv.FormatInt(head.GetSize(), 10))
	}
	if ranged && head.GetRanged() {
		start, n, ok := byterange.Resolve(offset, length, head.GetSize())
		if !ok {
			h.Del("Content-Length")
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", head.GetSize()))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, head.GetSize()))
		h.Set("Content-Length", strconv.FormatInt(n, 10))
		code = http.StatusPartialContent
		if firstRange(d.key + "|" + etag) {
			d.record("")
		}
	} else {
		d.record("")
	}
	w.WriteHeader(code)
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			// 已经开始发内容了，只能断开连接，让浏览器知道下载没有完成。
			panic(http.ErrAbortHandler)
		}
		if _, err := w.Write(c.GetData()); err != nil {
			return
		}
	}
}

// start 打开节点的下载流，取第一条（文件名、大小）。出错时已经回答了浏览器。
func (d fileDownload) start(ctx context.Context, offset, length int64) (chunkStream, *nagiv1.VolumeFileChunk, error) {
	stream, err := d.open(ctx, offset, length)
	if err != nil {
		d.record(activityFailure(err))
		d.fail(http.StatusBadGateway, err)
		return nil, nil, err
	}
	head, err := stream.Recv()
	if err != nil {
		d.record(activityFailure(err))
		d.fail(http.StatusBadRequest, err)
		return nil, nil, err
	}
	return stream, head, nil
}

// ifRangeMatches：没有 If-Range，或者它和现在的 ETag（或修改时间）一致。
func ifRangeMatches(ifRange, etag string, modified time.Time) bool {
	if ifRange == "" {
		return true
	}
	if etag != "" && ifRange == etag {
		return true
	}
	if t, err := http.ParseTime(ifRange); err == nil && !modified.IsZero() {
		return modified.Truncate(time.Second).Equal(t)
	}
	return false
}
