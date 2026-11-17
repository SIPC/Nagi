package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidateWebhookAddress(t *testing.T) {
	cases := []struct {
		url          string
		allowPrivate bool
		want         string // 空：通过；否则错误里要有这段文字
	}{
		{"https://hooks.example.com/x", false, ""},
		{"https://user:pass@hooks.example.com/x", false, ""},
		{"http://1.1.1.1:8080/x", false, ""},
		{"http://192.168.1.10/hook", false, "允许内网地址"},
		{"http://192.168.1.10/hook", true, ""},
		{"http://10.0.0.1/hook", false, "允许内网地址"},
		{"http://[fd12::1]/hook", false, "允许内网地址"},
		{"http://[fd12::1]/hook", true, ""},
		{"http://100.64.1.1/hook", false, "允许内网地址"},
		{"http://127.0.0.1:9000/", true, "不能使用"},
		{"http://[::1]/", true, "不能使用"},
		{"http://localhost:9000/", true, "不能使用"},
		{"http://a.localhost/", true, "不能使用"},
		{"http://169.254.169.254/latest/meta-data/", true, "不能使用"},
		{"http://100.100.100.200/", true, "不能使用"},
		{"http://0.0.0.0/", true, "不能使用"},
		{"ftp://example.com/", false, "http://"},
		{"https:///x", false, "http://"},
	}
	for _, c := range cases {
		err := Validate(&nagiv1.AlertChannel{Type: "webhook", Url: c.url, AllowPrivate: c.allowPrivate})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s（允许内网 %v）：不应该报错：%v", c.url, c.allowPrivate, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s（允许内网 %v）：想要包含 %q 的错误，得到 %v", c.url, c.allowPrivate, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "pass") {
			t.Errorf("错误里不应该有地址里的密码：%v", err)
		}
	}
}

func testAlert() *nagiv1.Alert {
	return &nagiv1.Alert{Key: "test", Status: "firing", Title: "t", Time: timestamppb.Now()}
}

// 用默认的客户端发到本机：域名在连接时检查，允许内网地址也发不出去，错误说明原因。
func TestSendBlocksLoopback(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/hook"
	for _, allowPrivate := range []bool{false, true} {
		err := Send(context.Background(), &nagiv1.AlertChannel{Type: "webhook", Url: url, AllowPrivate: allowPrivate}, testAlert())
		if err == nil || !strings.Contains(err.Error(), "不能使用") {
			t.Errorf("允许内网 %v：想要地址不能使用的错误，得到 %v", allowPrivate, err)
		}
	}
	if hit {
		t.Fatal("请求到达了本机的服务")
	}
}

func TestSendDoesNotEchoBodyOrFollowRedirects(t *testing.T) {
	AllowLoopbackForTest(t) // 换成能连本机的客户端（不跟随跳转，和正式的一样），检查响应的处理
	followed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("secret-internal-data"))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/target", func(http.ResponseWriter, *http.Request) { followed = true })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	send := func(path string) error {
		return Send(context.Background(), &nagiv1.AlertChannel{Type: "webhook", Url: srv.URL + path}, testAlert())
	}
	if err := send("/ok"); err != nil {
		t.Fatalf("2xx 应该算成功：%v", err)
	}
	err := send("/fail")
	if err == nil || err.Error() != "渠道返回 HTTP 500" {
		t.Fatalf("非 2xx 只报状态码，得到 %v", err)
	}
	err = send("/redirect")
	if err == nil || err.Error() != "渠道返回 HTTP 307" {
		t.Fatalf("跳转应该算失败，得到 %v", err)
	}
	if followed {
		t.Fatal("不应该跟随跳转")
	}
}

func TestSendTelegramErrors(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+token+"/sendMessage" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":false,"description":"response body"}`))
	}))
	defer srv.Close()
	old := telegramAPI
	telegramAPI = srv.URL
	t.Cleanup(func() { telegramAPI = old })
	ch := &nagiv1.AlertChannel{Type: "telegram", TelegramToken: token, TelegramChatId: "1"}
	for _, c := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "令牌无效"},
		{http.StatusBadRequest, "聊天 ID"},
		{http.StatusInternalServerError, "渠道返回 HTTP 500"},
		{http.StatusOK, ""},
	} {
		status = c.status
		err := Send(context.Background(), ch, testAlert())
		if c.want == "" {
			if err != nil {
				t.Errorf("HTTP %d：不应该报错：%v", c.status, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("HTTP %d：想要包含 %q，得到 %v", c.status, c.want, err)
		}
		if err != nil && (strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "response body")) {
			t.Errorf("错误里不应该有令牌或响应内容：%v", err)
		}
	}
}
