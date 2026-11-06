package agent

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUpgradeState：取消只在替换之前有效；取消后要等这次升级收尾才能再升级。
func TestUpgradeState(t *testing.T) {
	var u upgradeState
	if got := status.Code(u.abort()); got != codes.NotFound {
		t.Fatalf("没有在升级时取消应该返回 NotFound，实际 %v", got)
	}
	ctx, cancel, err := u.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, _, err := u.begin(context.Background()); status.Code(err) != codes.Aborted {
		t.Fatalf("同时升级两次应该返回 Aborted，实际 %v", err)
	}
	if err := u.abort(); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("取消后下载用的 ctx 应该结束")
	}
	if u.commit() {
		t.Fatal("取消后不应该替换")
	}
	if _, _, err := u.begin(context.Background()); status.Code(err) != codes.Aborted {
		t.Fatalf("取消的升级收尾之前不能再升级，实际 %v", err)
	}
	u.end()

	_, cancel2, err := u.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()
	if !u.commit() {
		t.Fatal("没有取消时应该可以替换")
	}
	if got := status.Code(u.abort()); got != codes.FailedPrecondition {
		t.Fatalf("开始替换以后取消应该返回 FailedPrecondition，实际 %v", got)
	}
}
