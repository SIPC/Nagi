package api

import (
	"context"
	"net/http"

	"github.com/SIPC/nagi/internal/pkg/filehttp"
)

// chunkStream 是节点发来的下载流（卷文件、节点文件）：第一条带文件名、大小，之后是内容。
type chunkStream = filehttp.ChunkStream

// fileDownload 是一次经面板从节点下载文件的请求（卷文件、节点文件）。下载本身见 filehttp.Download。
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

func (d fileDownload) serve(w http.ResponseWriter, r *http.Request) {
	filehttp.Download{
		Open: d.open,
		Record: func(err error) {
			if err != nil {
				d.record(activityFailure(err))
			} else {
				d.record("")
			}
		},
		Fail:  d.fail,
		Check: d.check,
		Key:   d.key,
	}.Serve(w, r)
}
