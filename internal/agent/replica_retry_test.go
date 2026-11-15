package agent

import (
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
