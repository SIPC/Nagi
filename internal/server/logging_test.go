package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func TestPanelAccessHistoryIncludesRejectedAndStreamingRequests(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	s := &Server{cfg: Config{Logs: logs}}
	h := s.accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nagi.v1.TestService/Watch":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusAccepted)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "stream-result")
		case "/secret-entrance":
			w.WriteHeader(http.StatusUnauthorized)
		case "/assets/app.js":
			http.Redirect(w, r, "/assets/new.js", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	for _, path := range []string{"/secret-entrance?token=do-not-store", "/unknown", "/assets/app.js", "/nagi.v1.TestService/Watch"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("hidden-body"))
		r.Header.Set("Authorization", "Bearer credential")
		r.Header.Set("X-Forwarded-For", "203.0.113.99") // 未获信任的头不能冒充真实来源。
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if path == "/nagi.v1.TestService/Watch" && (!w.Flushed || w.Body.String() != "stream-result" || w.Code != 202) {
			t.Fatalf("日志中间件破坏流响应: %+v", w)
		}
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Access})
	if err != nil || len(rows) != 4 {
		t.Fatalf("拒绝/未匹配/跳转/流请求没有全记: rows=%+v err=%v", rows, err)
	}
	want := []int{401, 404, 307, 202}
	for i, row := range rows {
		b := row.Data.(json.RawMessage)
		var data struct {
			Status   int
			BytesIn  int64  `json:"bytes_in"`
			BytesOut int64  `json:"bytes_out"`
			ClientIP string `json:"client_ip"`
		}
		if err := json.Unmarshal(b, &data); err != nil || data.Status != want[i] {
			t.Fatalf("真实响应码不对: %s err=%v", b, err)
		}
		if i == 3 && (data.BytesIn != 11 || data.BytesOut != 13) {
			t.Fatalf("实际收发字节不对: %s", b)
		}
		for _, secret := range []string{"secret-entrance", "do-not-store", "credential", "hidden-body", "203.0.113.99"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("秘密或不可信来源进库: %s", b)
			}
		}
	}
}

func TestPanelAccessHistoryDoesNotInventStatusAfterHijack(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	s := &Server{cfg: Config{Logs: logs}}
	h := s.accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = rw.Flush()
	}))
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(done)
	}))
	defer srv.Close()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("接管连接被日志破坏: response=%v err=%v", resp, err)
	}
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("接管后的日志没有完成")
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Access})
	if err != nil || len(rows) != 1 {
		t.Fatalf("接管请求没有入库: rows=%v err=%v", rows, err)
	}
	var data struct {
		Status              int
		Hijacked            bool
		ResponseStatusKnown bool `json:"response_status_known"`
	}
	if err := json.Unmarshal(rows[0].Data.(json.RawMessage), &data); err != nil || data.Status != 0 || !data.Hijacked || data.ResponseStatusKnown {
		t.Fatalf("没有观测到的状态不能假记 200: %+v err=%v", data, err)
	}
}
