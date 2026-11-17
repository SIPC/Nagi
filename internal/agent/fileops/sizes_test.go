package fileops

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 完整统计的名额被占满时最多等 fullWait，之后按快速统计的限额算（多半统计不完，界面显示「—」）；
// 等的时候调用方取消了就不再等；名额空出来以后照常拿到完整统计的限额。
func TestDirSizeLimitsWait(t *testing.T) {
	old := fullWait
	fullWait = 50 * time.Millisecond
	t.Cleanup(func() { fullWait = old })

	var releases []func()
	for range cap(fullSlots) {
		timeout, _, release, err := DirSizeLimits(context.Background(), true)
		if err != nil || timeout != DirSizeFullTimeout {
			t.Fatalf("有空名额时应拿到完整统计的限额：%v %v", timeout, err)
		}
		releases = append(releases, release)
	}

	start := time.Now()
	timeout, maxEntries, release, err := DirSizeLimits(context.Background(), true)
	if err != nil || timeout != DirSizeTimeout || maxEntries != DirSizeMaxEntries || release == nil {
		t.Fatalf("等不到名额时应按快速统计的限额：%v %v %v", timeout, maxEntries, err)
	}
	release()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("等得太久：%v", d)
	}
	if len(fullSlots) != cap(fullSlots) {
		t.Fatal("快速统计的 release 不应释放别人的名额")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := DirSizeLimits(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后应返回 ctx 的错误：%v", err)
	}

	for _, r := range releases {
		r()
	}
	timeout, _, release, err = DirSizeLimits(context.Background(), true)
	if err != nil || timeout != DirSizeFullTimeout {
		t.Fatalf("名额空出来以后应拿到完整统计的限额：%v %v", timeout, err)
	}
	release()
}
