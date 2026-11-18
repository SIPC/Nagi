package projects

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// 旧版本的源节点：每个（容器，备份节点）面板只在后台等一个；在等的时候再请求不再开协程，都在等时返回「已在同步」，
// 等完以后可以再同步。
func TestWaitOldSyncOnePerTarget(t *testing.T) {
	s := &Service{Log: slog.Default()}
	ctx := context.Background()
	release := make(chan struct{})
	var mu sync.Mutex
	calls := map[string]int{}
	finished := make(chan string, 10)
	call := func(_ context.Context, id string) error {
		mu.Lock()
		calls[id]++
		mu.Unlock()
		<-release
		finished <- id
		return nil
	}

	if err := s.waitOldSync(ctx, "p", "web", []string{"n1"}, call); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := s.waitOldSync(ctx, "p", "web", []string{"n1"}, call); err == nil || !strings.Contains(err.Error(), "已在同步") {
			t.Fatalf("在等时再请求：%v", err)
		}
	}
	// 一起请求 n1 和 n2：n1 在等，跳过；n2 开始。别的容器互不影响。
	if err := s.waitOldSync(ctx, "p", "web", []string{"n1", "n2"}, call); err != nil {
		t.Fatal(err)
	}
	if err := s.waitOldSync(ctx, "p", "db", []string{"n1"}, call); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range 3 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("同步没有结束")
		}
	}
	mu.Lock()
	if calls["n1"] != 2 || calls["n2"] != 1 {
		t.Errorf("调用次数：%v，应为 n1 两次（web、db 各一次）、n2 一次", calls)
	}
	mu.Unlock()

	// 等完以后释放：可以再同步。
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.oldSyncs)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("还有 %d 个在等", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.waitOldSync(ctx, "p", "web", []string{"n1"}, call); err != nil {
		t.Fatalf("等完以后：%v", err)
	}
	<-finished
}
