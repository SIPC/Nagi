package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/server/auth"
)

// accessLog 放在认证/安全入口之前，因此查询、静态文件、拒绝、404 和跳转都能留事实。
// 不读正文、不复制凭据/转发头，也不把 HTTP 状态当作已经证实的认证或攻击原因。
func (s *Server) accessLog(next http.Handler) http.Handler {
	if s.cfg.Logs == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		path := logstore.SafePanelPath(r.URL.Path)
		var body *requestCounter
		if r.Body != nil {
			body = &requestCounter{ReadCloser: r.Body}
			r.Body = body
		}
		cw := &accessWriter{ResponseWriter: w}
		completed := false
		defer func() {
			status := cw.status
			if status == 0 && completed && !cw.hijacked {
				status = http.StatusOK
			}
			var received int64
			if body != nil {
				received = body.bytes
			}
			err := ""
			if r.Context().Err() != nil {
				err = r.Context().Err().Error()
			}
			s.cfg.Logs.Report(logstore.Event{Time: start, Category: logstore.Access, Kind: "panel.http", Data: map[string]any{
				"method": r.Method, "path": path, "remote": r.RemoteAddr, "client_ip": auth.RequestIP(r.RemoteAddr, r.Header),
				"protocol": r.Proto, "status": status, "duration_ms": float64(time.Since(start)) / float64(time.Millisecond),
				"bytes_in": received, "bytes_out": cw.bytes, "interrupted": !completed, "context_error": err,
				"hijacked": cw.hijacked, "response_status_known": status != 0,
			}})
		}()
		next.ServeHTTP(cw, r)
		completed = true
	})
}

type requestCounter struct {
	io.ReadCloser
	bytes int64
}

func (r *requestCounter) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	return n, err
}

type accessWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *accessWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *accessWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true // 握手可能直接写到 c，没观测到的状态码/帧字节不能冒充 HTTP 200/0 字节。
	}
	return c, rw, err
}
