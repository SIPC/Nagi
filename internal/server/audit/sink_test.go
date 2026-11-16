package audit

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/auditsink"
)

// HTTP 目标收到记录和锚点；目标恢复以后从游标处补发。
func TestShipperHTTP(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)
	var mu sync.Mutex
	var got []map[string]any
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				got = append(got, m)
			}
		}
	}))
	defer srv.Close()
	sh := &Shipper{Log: l}
	sk := SinkConfig{ID: "h", Type: "http", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer k"}}
	sh.Configure(ctx, []SinkConfig{sk})
	defer sh.Stop()
	for range anchorEvery + 5 {
		l.Record(ctx, Event{Category: Operation, Action: "Deploy"})
	}
	time.Sleep(3 * time.Second)
	if st := sh.Status()["h"]; st.LastError == "" {
		t.Fatalf("目标不可用时应记下错误：%+v", st)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if !sh.Flush(int64(anchorEvery+5), 20*time.Second) {
		t.Fatalf("没有补发完：%+v", sh.Status())
	}
	mu.Lock()
	defer mu.Unlock()
	events, anchors := 0, 0
	for _, m := range got {
		if m["type"] == "anchor" {
			anchors++
		} else if m["hash"] != nil {
			events++
		}
	}
	if events != anchorEvery+5 || anchors == 0 {
		t.Fatalf("收到 %d 条记录、%d 个锚点", events, anchors)
	}
	if a, ok := l.LastAnchor(ctx); !ok || a.ID == 0 {
		t.Fatal("没有记下最近的锚点")
	}
}

// syslog 目标按 RFC 5425 分帧发出。
func TestShipperSyslog(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	cert := srv.TLS.Certificates
	srv.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: cert})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	recv := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		recv <- string(b)
	}()
	ca := string(certPEM(t, srv.Certificate()))
	err = auditsink.SendSyslog(context.Background(), SinkConfig{Type: "syslog", Address: ln.Addr().String(), CA: ca}, [][]byte{[]byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	msg := <-recv
	if !strings.Contains(msg, "<110>1 ") || !strings.Contains(msg, `{"a":1}`) {
		t.Fatalf("收到 %q", msg)
	}
}

func certPEM(t *testing.T, c *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}
