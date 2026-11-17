package filehttp

import (
	"context"
	"errors"
	"io"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// Pipe 按顺序交出所有内容，读完时返回 EOF，出错时在内容之后返回错误。
func TestPipe(t *testing.T) {
	for _, fail := range []error{nil, errors.New("磁盘坏了")} {
		s := Pipe(context.Background(), func(send func(*nagiv1.VolumeFileChunk) error) error {
			for i := range 20 {
				if err := send(&nagiv1.VolumeFileChunk{Data: []byte{byte(i)}}); err != nil {
					return err
				}
			}
			return fail
		})
		for i := range 20 {
			c, err := s.Recv()
			if err != nil || c.GetData()[0] != byte(i) {
				t.Fatalf("第 %d 块：%v %v", i, c, err)
			}
		}
		_, err := s.Recv()
		if fail == nil && err != io.EOF || fail != nil && err != fail {
			t.Errorf("结束：%v，想要 %v", err, fail)
		}
	}
	// 取消以后读文件的那一边不会一直卡着。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	Pipe(ctx, func(send func(*nagiv1.VolumeFileChunk) error) error {
		for {
			if err := send(&nagiv1.VolumeFileChunk{}); err != nil {
				done <- err
				return err
			}
		}
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("取消：%v", err)
	}
}
