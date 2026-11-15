package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

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

// 连接断了而且有进展就接着传；一点没传过去、对方拒绝或者到了次数上限就停。
func TestResumeSync(t *testing.T) {
	old := syncRetryDelay
	syncRetryDelay = 0
	t.Cleanup(func() { syncRetryDelay = old })
	lost := status.Error(codes.Unavailable, "received a stateless reset")
	run := func(results ...any) (int64, error, int) {
		calls := 0
		total, err := resumeSync(context.Background(), func() (int64, error) {
			n, e := results[2*calls].(int64), results[2*calls+1]
			calls++
			if e == nil {
				return n, nil
			}
			return n, e.(error)
		}, func(int, int64, error) {})
		return total, err, calls
	}
	if total, err, calls := run(int64(100), lost, int64(50), lost, int64(30), nil); err != nil || total != 180 || calls != 3 {
		t.Errorf("断了两次后传完：total=%d err=%v calls=%d", total, err, calls)
	}
	if _, err, calls := run(int64(100), lost, int64(0), lost); err == nil || calls != 2 {
		t.Errorf("重试一点没传过去应停下：err=%v calls=%d", err, calls)
	}
	if _, err, calls := run(int64(0), status.Error(codes.PermissionDenied, "no")); err == nil || calls != 1 {
		t.Errorf("对方拒绝不重试：err=%v calls=%d", err, calls)
	}
	many := []any{}
	for i := 0; i < syncAttempts+2; i++ {
		many = append(many, int64(10), lost)
	}
	if _, err, calls := run(many...); err == nil || calls != syncAttempts {
		t.Errorf("最多 %d 次：calls=%d", syncAttempts, calls)
	}
}
