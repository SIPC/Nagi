package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 同步到备份节点的进度（见 docs/snapshot.md#同步的进度）：源节点记着每次同步要传多少、传了多少、最近的速度，
// 随容器状态上报（BackupStatus.bytes_total 等）。同步在节点自己的上下文里跑完，面板只是请它开始（SyncBackupRequest.async）。

// speedWindow 是算速度用的时间窗口；progressInterval 是同步进行时多久上报一次进度。
const (
	speedWindow      = 20 * time.Second
	progressInterval = 5 * time.Second
)

// errSyncStopped 是用户停止同步时取消上下文的原因。
var errSyncStopped = errors.New("已停止同步")

// syncProgress 是一次同步的进度。total 是这次要传的字节数，done 是已经传了的（重试时按对方已有的重新算）。
type syncProgress struct {
	total atomic.Int64
	done  atomic.Int64

	mu       sync.Mutex
	started  time.Time // 真正开始传的时间；排队等前面的备份节点时为零
	samples  []progressSample
	speed    int64
	retries  uint32
	retryErr string
}

type progressSample struct {
	at   time.Time
	done int64
}

// begin 记下真正开始同步的时间。
func (p *syncProgress) begin(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.IsZero() {
		p.started = now
	}
}

// plan 在每次连接核对清单以后调用：need 是这次还要传的字节数。第一次就是总量；之后已传的按总量减去还要传的算，
// 断开时没传完的部分不算进去；有新文件、要传的比总量还多时总量跟着变大。
func (p *syncProgress) plan(need int64) {
	total := p.total.Load()
	if need > total {
		p.total.Store(need)
		total = need
	}
	p.done.Store(total - need)
}

// add 记下又传了 n 字节。
func (p *syncProgress) add(n int64) { p.done.Add(n) }

// retried 记下一次连接断开后的重试。
func (p *syncProgress) retried(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retries++
	p.retryErr = err.Error()
}

// sample 记下现在传了多少，按最近 speedWindow 算速度。已传的变少了（重试时重新算过）就从头算。
func (p *syncProgress) sample(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	done := p.done.Load()
	if n := len(p.samples); n > 0 && done < p.samples[n-1].done {
		p.samples = p.samples[:0]
	}
	p.samples = append(p.samples, progressSample{now, done})
	// 留一个窗口以外的作为起点。
	for len(p.samples) > 2 && now.Sub(p.samples[1].at) >= speedWindow {
		p.samples = p.samples[1:]
	}
	first := p.samples[0]
	if d := now.Sub(first.at); d >= time.Second {
		p.speed = int64(float64(done-first.done) / d.Seconds())
	} else {
		p.speed = 0
	}
}

// fill 把进度写进上报的状态。
func (p *syncProgress) fill(st *nagiv1.BackupStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.IsZero() {
		return
	}
	st.StartedAt = timestamppb.New(p.started)
	st.BytesTotal, st.BytesDone = p.total.Load(), min(p.done.Load(), p.total.Load())
	st.BytesPerSecond = p.speed
	st.Retries, st.RetryError = p.retries, p.retryErr
}

// transferLog 不拿副本/状态锁写库；完成时先停止并等待进度采集，结束之后不会再出现进度。
// 取消时仍独立保存结果；不采集文件清单/正文、仓库配置或目标公钥。
func (a *agent) transferLog(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget, p *syncProgress) (func(int, int64, error), func(int64, error)) {
	if a.cfg.Logs == nil {
		return func(int, int64, error) {}, func(int64, error) {}
	}
	id, started := rand.Text(), time.Now()
	record := func(phase string, attempt int, sent int64, err error) {
		var st nagiv1.BackupStatus
		p.fill(&st)
		data := map[string]any{"task_id": id, "phase": phase, "target_node": target.GetNodeId(), "target_name": target.GetName(),
			"bytes_total": st.BytesTotal, "bytes_done": st.BytesDone, "retries": st.Retries}
		if phase == "finished" {
			data["result"], data["duration_ms"], data["transferred_bytes"] = "ok", time.Since(started).Milliseconds(), sent
			if err != nil {
				data["result"] = "failed"
			}
			if err != nil && ctx.Err() != nil {
				data["result"], data["cancel_cause"] = "interrupted", context.Cause(ctx).Error()
				if errors.Is(context.Cause(ctx), errSyncStopped) {
					data["result"] = "stopped"
				}
			}
		}
		if phase == "retry" {
			data["attempt"], data["attempt_bytes"] = attempt, sent
		}
		if err != nil {
			data["error"] = err.Error()
		}
		a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "replica.transfer", Project: projectID, Object: container, Data: data})
	}
	record("started", 0, 0, nil)
	seconds := a.cfg.LogConfig.TransferProgressSeconds
	if seconds == 0 {
		seconds = logstore.DefaultConfig().TransferProgressSeconds
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(seconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				record("progress", 0, 0, nil)
			}
		}
	}()
	return func(attempt int, sent int64, err error) { record("retry", attempt, sent, err) }, func(sent int64, err error) {
		close(stop)
		<-done
		record("finished", 0, sent, err)
	}
}

// syncRun 是一个容器到一个备份节点正在进行（或排着队）的同步。ctx 是节点自己的上下文派生的，
// 不随发起请求的面板连接取消，只在用户停止（errSyncStopped）或节点退出时取消。
type syncRun struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	progress *syncProgress
	// again：同步进行中又有人请求（新的快照、「立即同步」），这次结束后再同步一次。
	again bool
	// done 在这个备份节点的同步（包括 again 的那次）都结束后关闭。
	done chan struct{}
}

func newSyncRun(parent context.Context) *syncRun {
	ctx, cancel := context.WithCancelCause(parent)
	return &syncRun{ctx: ctx, cancel: cancel, progress: &syncProgress{}, done: make(chan struct{})}
}

// claim 让容器 id 到 target 的同步开始：没有在同步时登记一个新的返回；已经在同步时让它结束后再来一次，返回 nil。
func (b *backupStates) claim(parent context.Context, id, target string) *syncRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entry(id, target)
	if e.run != nil {
		e.run.again = true
		return nil
	}
	e.run = newSyncRun(parent)
	return e.run
}

// running 返回容器 id 到 target 正在进行的同步，没有时返回 nil。
func (b *backupStates) running(id, target string) *syncRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entry(id, target).run
}

// stop 停止容器 id 到 target 的同步（也不再来一次），返回有没有在同步。
func (b *backupStates) stop(id, target string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entry(id, target)
	if e.run == nil {
		return false
	}
	e.run.again = false
	e.run.cancel(errSyncStopped)
	return true
}

// tick 给正在传的同步记一次速度，返回有没有正在进行的同步。
func (b *backupStates) tick(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	active := false
	for _, e := range b.m {
		if e.run != nil {
			e.run.progress.sample(now)
			active = true
		}
	}
	return active
}

// backupProgressLoop 在有同步进行时每 progressInterval 记一次速度并上报一次，界面上的进度跟着动。
func (a *agent) backupProgressLoop(ctx context.Context) {
	t := time.NewTicker(progressInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if a.backups.tick(now) {
				a.notifyReport()
			}
		}
	}
}

// resumeInterruptedSyncs 在启动后接着做上次节点退出时没做完的同步（例如第一次同步做到一半节点升级重启）。
func (a *agent) resumeInterruptedSyncs(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(resumeDelay):
	}
	for _, c := range a.projects.List(ctx) {
		s := c.Spec
		id := s.GetProjectId() + "-" + s.GetName()
		for _, t := range backupTargets(s) {
			a.backups.mu.Lock()
			interrupted := a.backups.entry(id, t.GetNodeId()).Interrupted
			a.backups.mu.Unlock()
			if interrupted {
				a.log.Info("接着同步上次中断的备份", "project", s.GetProjectId(), "container", s.GetName(), "target", t.GetName())
				_ = a.startBackupSync(ctx, s.GetProjectId(), s.GetName(), t.GetNodeId())
			}
		}
	}
}

// resumeDelay：启动后等节点网络连上别的节点再接着同步。
var resumeDelay = 30 * time.Second
