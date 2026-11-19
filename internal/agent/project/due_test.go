package project

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 一个容器的定时快照卡住（v0.35.0#7：kopia 卡在容器换上的 FIFO 的 open(2) 里，不响应取消），
// 别的容器照常做完；卡住的那个超时以后报告失败，没返回之前不再开始它的下一轮，返回以后再报告真正的结果。
func TestDueRunsStuckContainerDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	d := &dueRuns{timeout: 100 * time.Millisecond, grace: 100 * time.Millisecond, parallel: 1}

	type result struct {
		key, task string
		err       error
	}
	results := make(chan result, 10)
	reporter := func(key string) func(string, error) {
		return func(task string, err error) { results <- result{key, task, err} }
	}
	next := func() result {
		t.Helper()
		select {
		case r := <-results:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no result within 5s")
			return result{}
		}
	}

	// a 卡住，不理会 ctx。
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	stuck := dueTask{"snapshot", func(context.Context) error { <-release; return nil }}
	if !d.start(ctx, "a", []dueTask{stuck}, reporter("a")) {
		t.Fatal("a should start")
	}
	// 只有一个并发名额，b 要等 a 卡住、让出名额以后才能运行。
	ran := make(chan struct{})
	ok := dueTask{"snapshot", func(context.Context) error { close(ran); return nil }}
	maint := dueTask{"maintain", func(context.Context) error { return errors.New("boom") }}
	if !d.start(ctx, "b", []dueTask{ok, maint}, reporter("b")) {
		t.Fatal("b should start")
	}

	got := map[string]result{}
	for range 3 {
		r := next()
		got[r.key+"/"+r.task] = r
	}
	if r := got["a/snapshot"]; r.err == nil || !strings.Contains(r.err.Error(), "卡住") {
		t.Fatalf("stuck container should be reported as failed, got %v", r.err)
	}
	if r, seen := got["b/snapshot"]; !seen || r.err != nil {
		t.Fatalf("other container's snapshot should succeed: %+v", got)
	}
	if r := got["b/maintain"]; r.err == nil {
		t.Fatalf("maintain error should be reported: %+v", got)
	}
	select {
	case <-ran:
	default:
		t.Fatal("b never ran")
	}

	// a 还没返回，下一轮不开始；b 已经结束，可以再开始。
	if d.start(ctx, "a", []dueTask{stuck}, reporter("a")) {
		t.Fatal("a must not start again while the previous run is still stuck")
	}
	noop := dueTask{"snapshot", func(context.Context) error { return errNotDue }}
	waitIdle := func(key string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			d.mu.Lock()
			busy := d.busy[key]
			d.mu.Unlock()
			if !busy {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s still busy", key)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitIdle("b")
	if !d.start(ctx, "b", []dueTask{noop}, reporter("b")) {
		t.Fatal("b should start again after finishing")
	}

	// a 终于返回：报告真正的结果，之后可以再开始。
	releaseOnce.Do(func() { close(release) })
	if r := next(); r.key != "a" || r.err != nil {
		t.Fatalf("late result of a = %+v", r)
	}
	waitIdle("a")
	if !d.start(ctx, "a", []dueTask{noop}, reporter("a")) {
		t.Fatal("a should start again after it returned")
	}
	waitIdle("a")
	waitIdle("b")
	select {
	case r := <-results:
		t.Fatalf("errNotDue must not be reported: %+v", r)
	default:
	}
}

// 响应取消的任务超时以后按失败报告，写明超时。
func TestDueRunsTimeout(t *testing.T) {
	d := &dueRuns{timeout: 50 * time.Millisecond, grace: time.Minute}
	done := make(chan error, 1)
	slow := dueTask{"snapshot", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	d.start(context.Background(), "a", []dueTask{slow}, func(_ string, err error) { done <- err })
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "已取消") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout not reported")
	}
}
