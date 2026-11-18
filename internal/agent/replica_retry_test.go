package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 连接断了（包了一层说明也一样）才重试；对方拒绝、本地出错不重试。
func TestConnectionLost(t *testing.T) {
	reset := status.Error(codes.Unavailable, "error reading from server: received a stateless reset")
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("传输文件: %w", reset), true},
		{reset, true},
		{fmt.Errorf("提交: %w", status.Error(codes.FailedPrecondition, "no")), false},
		{errors.New("节点网络没有启动"), false},
	}
	for _, c := range cases {
		if got := connectionLost(c.err); got != c.want {
			t.Errorf("%v：得到 %v", c.err, got)
		}
	}
}

// 连接断了而且有进展（还要传的比以前少）就接着传；连续 syncStallAttempts 次没有进展、对方拒绝、ctx 取消就停。
func TestResumeSync(t *testing.T) {
	oldDelay, oldMax := syncRetryDelay, syncRetryMax
	syncRetryDelay, syncRetryMax = 0, 0
	t.Cleanup(func() { syncRetryDelay, syncRetryMax = oldDelay, oldMax })
	lost := status.Error(codes.Unavailable, "received a stateless reset")
	type step struct {
		sent, need int64
		err        error
	}
	run := func(ctx context.Context, steps ...step) (int64, error, int) {
		calls := 0
		total, err := resumeSync(ctx, func() (int64, int64, error) {
			s := steps[min(calls, len(steps)-1)]
			calls++
			return s.sent, s.need, s.err
		}, func(int, int64, error) {})
		return total, err, calls
	}
	ctx := context.Background()
	if total, err, calls := run(ctx, step{100, 180, lost}, step{50, 80, lost}, step{30, 30, nil}); err != nil || total != 180 || calls != 3 {
		t.Errorf("断了两次后传完：total=%d err=%v calls=%d", total, err, calls)
	}
	// 一直没有进展：第一次算有（记下 need），之后连续 syncStallAttempts 次没变少就停。
	if _, err, calls := run(ctx, step{100, 500, lost}); err == nil || calls != syncStallAttempts+1 {
		t.Errorf("没有进展应停下：err=%v calls=%d", err, calls)
	}
	// 清单都拿不到（连不上）也算没有进展。
	if _, err, calls := run(ctx, step{0, -1, lost}); err == nil || calls != syncStallAttempts {
		t.Errorf("连不上应停下：err=%v calls=%d", err, calls)
	}
	// 有进展就一直接着传，不限次数。
	steps := []step{}
	for i := range 3 * syncStallAttempts {
		steps = append(steps, step{10, int64(1000 - i), lost})
	}
	steps = append(steps, step{10, 1, nil})
	if _, err, calls := run(ctx, steps...); err != nil || calls != len(steps) {
		t.Errorf("一直有进展应传完：err=%v calls=%d", err, calls)
	}
	if _, err, calls := run(ctx, step{0, 10, status.Error(codes.PermissionDenied, "no")}); err == nil || calls != 1 {
		t.Errorf("对方拒绝不重试：err=%v calls=%d", err, calls)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err, calls := run(canceled, step{10, 10, lost}); err == nil || calls != 1 {
		t.Errorf("取消后不重试：err=%v calls=%d", err, calls)
	}
}

// 传了一半的文件对不上、被对方丢掉（DataLoss）也接着传，下次从头传。
func TestConnectionLostDataLoss(t *testing.T) {
	if !connectionLost(fmt.Errorf("传输文件: %w", status.Error(codes.DataLoss, "对不上"))) {
		t.Fatal("DataLoss 应该接着传")
	}
}

// 对方太久没收到数据（旧版本的对方一次只收一条流时会这样）也算传输断了，接着传。
func TestConnectionLostStall(t *testing.T) {
	if !connectionLost(fmt.Errorf("传输文件: %w", status.Error(codes.DeadlineExceeded, "同步副本时长时间没有收到数据"))) {
		t.Fatal("DeadlineExceeded 应该接着传")
	}
}

// 同一个容器到同一个目标的同步排队；不同目标互不影响；等的时候可以取消。
func TestLockSync(t *testing.T) {
	ctx := context.Background()
	unlock, err := lockSync(ctx, "p/c/a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := lockSync(ctx, "p/c/b")
	if err != nil {
		t.Fatal("不同目标不应该等")
	}
	other()
	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := lockSync(waiting, "p/c/a"); err == nil {
		t.Fatal("同一个目标应该等前一次结束")
	}
	unlock()
	again, err := lockSync(ctx, "p/c/a")
	if err != nil {
		t.Fatal(err)
	}
	again()
}
