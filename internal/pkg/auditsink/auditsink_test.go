package auditsink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// L4：接收端返回 307 时不跟随（否则审计正文和自定义请求头会发到重定向的地址）；S3 出错时只带错误码，
// 不回显响应体（地址指向内网服务时，响应体会经测试外送和失败原因被读出来）。
func TestNoRedirectAndNoEcho(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "" {
			leaked.Add(1)
		}
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	ctx := context.Background()
	c := NewClient(5 * time.Second)
	err := PostHTTP(ctx, c, Config{Type: "http", URL: redirect.URL, Headers: map[string]string{"X-Token": "secret"}}, [][]byte{[]byte(`{}`)})
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("不应跟随重定向：err=%v，泄露 %d 次", err, leaked.Load())
	}

	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>internal-secret-data</Message></Error>`))
	}))
	defer internal.Close()
	err = S3Put(ctx, c, Config{Endpoint: internal.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}, "k", []byte("x"), "text/plain")
	if err == nil || strings.Contains(err.Error(), "internal-secret-data") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("S3 错误只应带错误码：%v", err)
	}
}
