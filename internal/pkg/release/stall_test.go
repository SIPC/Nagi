package release

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// stalledBody 的正文一直不来；请求被取消以后读到的是结尾，不是错误。
type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, io.EOF
}

func (stalledBody) Close() error { return nil }

type stalledTransport struct{}

func (stalledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		ContentLength: -1,
		Body:          stalledBody{r.Context()},
		Request:       r,
	}, nil
}

// 服务器回了 200、正文卡住，下载因为太慢或超时被取消时，读到的空内容不能当成文件：
// fetch 要报错，githubSums 要当作没取到（之后照常换加速代理），而不是「GitHub 上没有这个文件」。
func TestFetchStalledBody(t *testing.T) {
	oldT, oldTime, oldPin := transport, lowSpeedTime, pinTimeout
	transport, lowSpeedTime, pinTimeout = stalledTransport{}, 50*time.Millisecond, 5*time.Second
	t.Cleanup(func() { transport, lowSpeedTime, pinTimeout = oldT, oldTime, oldPin })

	const base = "https://github.invalid/SIPC/Nagi/releases/download/v0.1.0"
	ctx := context.Background()
	if b, err := fetch(ctx, base+"/SHA256SUMS", 64<<10, nil, nil); err == nil {
		t.Fatalf("读进内存：正文卡住时应当报错，得到 %q", b)
	}
	var buf bytes.Buffer
	if _, err := fetch(ctx, base+"/nagi-linux-amd64", 0, &buf, nil); err == nil {
		t.Fatal("写进文件：正文卡住时应当报错")
	}

	sources := []Source{{Base: base}, {Base: "https://proxy.invalid/" + base, Proxy: "https://proxy.invalid/"}}
	check := func(t *testing.T) {
		t.Helper()
		sum, err := githubSums(ctx, sources)
		if err != nil || sum != "" {
			t.Fatalf("githubSums = %q, %v，应当当作没取到", sum, err)
		}
	}
	t.Run("太慢", check)
	t.Run("超时", func(t *testing.T) {
		lowSpeedTime, pinTimeout = 5*time.Second, 50*time.Millisecond
		check(t)
	})
}
