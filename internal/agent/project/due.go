package project

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 定时任务（定时快照、仓库维护）每个容器各自在一个协程里跑，带超时（见 docs/snapshot.md#失败与退避）：
// 一个容器卡住（例如本地读写不响应取消）只影响它自己，别的容器照常按时快照。

const (
	// dueTimeout 是一个定时任务最多用多久，到时间取消并按失败处理（失败时写好的内容下次复用）。
	dueTimeout = 6 * time.Hour
	// dueGrace 是取消以后再等多久：还没返回就当作卡住了，先报告失败，并让出并发名额。
	dueGrace = time.Minute
	// dueParallel 是同时运行定时任务的容器数，免得到点时所有容器一起读盘。
	dueParallel = 4
)

// dueTask 是一个容器的一项定时任务，name 是 "snapshot" 或 "maintain"。返回 errNotDue 表示不用做，不报告。
type dueTask struct {
	name string
	run  func(ctx context.Context) error
}

// dueRuns 记着哪些容器的定时任务还在运行。零值可用。
type dueRuns struct {
	mu   sync.Mutex
	busy map[string]bool
	sem  chan struct{}

	// 测试用，0 时用上面的默认值。
	timeout, grace time.Duration
	parallel       int
}

// start 在一个新协程里依次运行容器 key 的定时任务，每项结束（或者卡住）时调用 report。
// 这个容器上一轮的任务还没返回（可能卡住了）时不开始，返回 false。
func (d *dueRuns) start(ctx context.Context, key string, tasks []dueTask, report func(task string, err error)) bool {
	d.mu.Lock()
	if d.busy[key] {
		d.mu.Unlock()
		return false
	}
	if d.busy == nil {
		d.busy = map[string]bool{}
	}
	if d.sem == nil {
		d.sem = make(chan struct{}, cmp.Or(d.parallel, dueParallel))
	}
	d.busy[key] = true
	sem := d.sem
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.busy, key)
			d.mu.Unlock()
		}()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var once sync.Once
		release := func() { once.Do(func() { <-sem }) }
		defer release()
		for _, t := range tasks {
			if ctx.Err() != nil {
				return
			}
			d.run(ctx, t, report, release)
		}
	}()
	return true
}

// run 运行一项任务，超过 timeout 取消；取消以后 grace 内还没返回，先报告卡住并让出并发名额（release），
// 等它真正返回时再报告一次结果。
func (d *dueRuns) run(ctx context.Context, t dueTask, report func(string, error), release func()) {
	timeout, grace := cmp.Or(d.timeout, dueTimeout), cmp.Or(d.grace, dueGrace)
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stuckDone := make(chan struct{})
	stuck := time.AfterFunc(timeout+grace, func() {
		defer close(stuckDone)
		release()
		report(t.name, fmt.Errorf("超过 %s 仍未完成，可能卡住了；结束之前不再开始这个容器的定时任务", durationText(timeout)))
	})
	err := t.run(tctx)
	if !stuck.Stop() {
		// 已经报告了卡住，等那次报告发完，真正的结果排在它后面。
		<-stuckDone
	}
	if errors.Is(err, errNotDue) {
		return
	}
	if err != nil && errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("超过 %s 未完成，已取消：%w", durationText(timeout), err)
	}
	report(t.name, err)
}

// durationText 把时长写成「6 小时」「30 分钟」，不整的照 Go 的格式。
func durationText(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", d/time.Minute)
	}
	return d.String()
}
