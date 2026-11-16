package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/auditsink"
)

// H2：面板和节点外送到同一个 S3 目标时，键要分开。之前面板第一条 ID 是 42 的批次和
// 节点本地序号是 42 的批次在同一小时写到同一个键，后写的覆盖先写的。
func TestS3KeysSeparatePanelAndNodes(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		puts = append(puts, r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	sk := SinkConfig{ID: "s", Type: "s3", Endpoint: srv.URL, Bucket: "b", Prefix: "nagi/", AccessKey: "a", SecretKey: "s"}
	s := &Shipper{client: srv.Client()}
	ctx := context.Background()
	if err := s.deliver(ctx, sk, []Event{{ID: 42}}, [][]byte{[]byte(`{"panel":1}`)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"n1", "n2", "../panel"} {
		if err := auditsink.Send(ctx, srv.Client(), sk, auditsink.NodeScope(id), 42, [][]byte{[]byte(`{"node":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TestSink(ctx, sk); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 5 {
		t.Fatalf("puts = %v", puts)
	}
	seen := map[string]bool{}
	for i, p := range puts {
		if seen[p] {
			t.Errorf("键重复：%s", p)
		}
		seen[p] = true
		want := []string{"/b/nagi/panel/", "/b/nagi/node/n1/", "/b/nagi/node/n2/", "/b/nagi/node/panel/", "/b/nagi/panel/nagi-test.json"}[i]
		if !strings.HasPrefix(p, want) {
			t.Errorf("第 %d 个键 %s，应该以 %s 开头", i, p, want)
		}
		if strings.Contains(p, "..") {
			t.Errorf("键里不能有 ..：%s", p)
		}
	}
}
