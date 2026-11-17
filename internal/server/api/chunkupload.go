package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 分块上传（断点续传，见 docs/agent.md#存储）：/api/volume-file 和 /api/node-file 上带 op 参数的请求。
//
//	POST   ?op=begin&size=&key=     开始或接着一个上传，返回 {id, offset}：offset 是节点已经收到的长度
//	PUT    ?op=part&id=&offset=     一块内容（不超过 16 MiB），请求头 X-Nagi-Chunk-Sha256 是这一块的 SHA-256，返回 {offset}
//	POST   ?op=commit&id=&sha256=   核对整个文件的大小和 SHA-256，写到目标位置，返回 {size}
//	DELETE ?op=abort&id=            放弃，删掉已经收到的内容
//
// 内容经 mesh 传给节点，节点按块写进暂存目录，记着到哪里为止都核对过了，所以断线、刷新页面以后再选同一个文件能接着传。
// 大小只受节点磁盘的剩余空间限制。提交（成功或失败）记进审计，和整个文件的上传同一个名字。

// chunkBody 是一块请求体的上限：一块 16 MiB，留一点余量给判断超长。
const chunkBody = 16<<20 + 1

type partStream interface {
	Send(*nagiv1.UploadPartChunk) error
	CloseAndRecv() (*nagiv1.UploadSession, error)
}

type chunkUploader struct {
	begin  func(ctx context.Context, size int64, key string) (*nagiv1.UploadSession, error)
	part   func(ctx context.Context) (partStream, error)
	first  func(id string, offset int64, sum string) *nagiv1.UploadPartChunk
	commit func(ctx context.Context, id, sum string) (*nagiv1.VolumeUploadResponse, error)
	abort  func(ctx context.Context, id string) error
	// record 记一次上传的结果（失败时带原因）。
	record func(failure string)
}

// uploadBusy 和节点的 agent.UploadBusy 相同（面板不引用节点的包）：节点的上传名额满了。
const uploadBusy = "节点正在接收其他上传，请稍后重试"

// chunkFail 写错误：除了说明，还带上节点返回的错误码，界面据此决定重试、重新开始还是放弃。
func chunkFail(w http.ResponseWriter, r *http.Request, code int, err error) {
	lang := requestLang(r)
	msg := errorText(lang, err)
	reason := ""
	if isStatus(err) {
		c := status.Code(err)
		reason = strings.ToLower(c.String())
		switch c {
		case codes.Unavailable, codes.DeadlineExceeded:
			msg = i18n.Localize(lang, i18n.New("无法连接节点，请稍后重试"))
		case codes.Unimplemented:
			msg = i18n.Localize(lang, i18n.New("节点版本过低，升级后才能使用此功能"))
		case codes.Aborted:
			code = http.StatusConflict
		case codes.NotFound:
			code = http.StatusNotFound
		case codes.ResourceExhausted:
			code = http.StatusInsufficientStorage
			if status.Convert(err).Message() == uploadBusy {
				code = http.StatusTooManyRequests
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg, "code": reason})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (c chunkUploader) serve(w http.ResponseWriter, r *http.Request, op string) {
	ctx := r.Context()
	q := r.URL.Query()
	switch {
	case op == "begin" && r.Method == http.MethodPost:
		size, err := strconv.ParseInt(q.Get("size"), 10, 64)
		if err != nil || size < 0 || q.Get("key") == "" {
			chunkFail(w, r, http.StatusBadRequest, i18n.New("上传参数无效"))
			return
		}
		sess, err := c.begin(ctx, size, q.Get("key"))
		if err != nil {
			c.record(activityFailure(err))
			chunkFail(w, r, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"id": sess.GetId(), "offset": sess.GetOffset()})
	case op == "part" && r.Method == http.MethodPut:
		offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
		sum := r.Header.Get("X-Nagi-Chunk-Sha256")
		if err != nil || offset < 0 || q.Get("id") == "" || sum == "" {
			chunkFail(w, r, http.StatusBadRequest, i18n.New("上传参数无效"))
			return
		}
		res, err := c.sendPart(ctx, w, r, q.Get("id"), offset, sum)
		if err != nil {
			chunkFail(w, r, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"offset": res.GetOffset()})
	case op == "commit" && r.Method == http.MethodPost:
		res, err := c.commit(ctx, q.Get("id"), q.Get("sha256"))
		if err != nil {
			c.record(activityFailure(err))
			chunkFail(w, r, http.StatusBadRequest, err)
			return
		}
		c.record("")
		writeJSON(w, map[string]any{"size": res.GetSize()})
	case op == "abort" && r.Method == http.MethodDelete:
		if err := c.abort(ctx, q.Get("id")); err != nil {
			chunkFail(w, r, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{})
	default:
		chunkFail(w, r, http.StatusBadRequest, i18n.New("上传参数无效"))
	}
}

// sendPart 把请求体作为一块转给节点。
func (c chunkUploader) sendPart(ctx context.Context, w http.ResponseWriter, r *http.Request, id string, offset int64, sum string) (*nagiv1.UploadSession, error) {
	stream, err := c.part(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(c.first(id, offset, sum)); err != nil {
		_, err = stream.CloseAndRecv()
		return nil, err
	}
	body := http.MaxBytesReader(w, r.Body, chunkBody)
	buf := make([]byte, 256<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if err := stream.Send(&nagiv1.UploadPartChunk{Data: buf[:n]}); err != nil {
				break // 节点提前结束（例如拒绝），真正的原因在 CloseAndRecv 里。
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			var tooBig *http.MaxBytesError
			if errors.As(rerr, &tooBig) {
				return nil, i18n.New("一块不能超过 16 MiB")
			}
			return nil, rerr // 浏览器断开：这一块作废，节点不会记下它
		}
	}
	return stream.CloseAndRecv()
}

// volumeChunks 是卷文件的分块上传。
func volumeChunks(client nagiv1.MeshServiceClient, ref *nagiv1.VolumeFileRef, record func(string)) chunkUploader {
	return chunkUploader{
		begin: func(ctx context.Context, size int64, key string) (*nagiv1.UploadSession, error) {
			return client.VolumeUploadBegin(ctx, &nagiv1.UploadBeginRequest{Volume: ref, Size: size, Key: key})
		},
		part: func(ctx context.Context) (partStream, error) { return client.VolumeUploadPart(ctx) },
		first: func(id string, offset int64, sum string) *nagiv1.UploadPartChunk {
			return &nagiv1.UploadPartChunk{Volume: ref, Id: id, Offset: offset, Sha256: sum}
		},
		commit: func(ctx context.Context, id, sum string) (*nagiv1.VolumeUploadResponse, error) {
			return client.VolumeUploadCommit(ctx, &nagiv1.UploadCommitRequest{Volume: ref, Id: id, Sha256: sum})
		},
		abort: func(ctx context.Context, id string) error {
			_, err := client.VolumeUploadAbort(ctx, &nagiv1.UploadAbortRequest{Volume: ref, Id: id})
			return err
		},
		record: record,
	}
}

// nodeChunks 是节点文件的分块上传。
func nodeChunks(client nagiv1.MeshServiceClient, ref *nagiv1.NodeFileRef, confirmed bool, record func(string)) chunkUploader {
	return chunkUploader{
		begin: func(ctx context.Context, size int64, key string) (*nagiv1.UploadSession, error) {
			return client.NodeFileUploadBegin(ctx, &nagiv1.UploadBeginRequest{Node: ref, Size: size, Key: key, Confirmed: confirmed})
		},
		part: func(ctx context.Context) (partStream, error) { return client.NodeFileUploadPart(ctx) },
		first: func(id string, offset int64, sum string) *nagiv1.UploadPartChunk {
			return &nagiv1.UploadPartChunk{Node: ref, Id: id, Offset: offset, Sha256: sum}
		},
		commit: func(ctx context.Context, id, sum string) (*nagiv1.VolumeUploadResponse, error) {
			return client.NodeFileUploadCommit(ctx, &nagiv1.UploadCommitRequest{Node: ref, Id: id, Sha256: sum, Confirmed: confirmed})
		},
		abort: func(ctx context.Context, id string) error {
			_, err := client.NodeFileUploadAbort(ctx, &nagiv1.UploadAbortRequest{Node: ref, Id: id})
			return err
		},
		record: record,
	}
}
