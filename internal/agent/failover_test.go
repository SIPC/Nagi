package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 同时发给同一个成员的投票合并成一次请求，结果按顺序回到各自的调用方。
func TestVoteBatcherMerges(t *testing.T) {
	var calls atomic.Int32
	b := &voteBatcher{send: func(_ context.Context, _ mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		calls.Add(1)
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i, r := range reqs {
			out[i] = &nagiv1.LeaseVoteResponse{Granted: true, Epoch: r.GetEpoch()}
		}
		return out, nil
	}}
	var k1, k2 mesh.Key
	k2[0] = 1
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := k1
			if i%2 == 1 {
				k = k2
			}
			resp, err := b.vote(context.Background(), k, &nagiv1.LeaseVoteRequest{ProjectId: "p", Container: "c", Epoch: uint64(i)})
			if err != nil || resp.GetEpoch() != uint64(i) {
				t.Errorf("投票 %d：%v %v", i, resp, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("发给两个成员的 10 个投票应该合并成 2 次请求，实际 %d 次", n)
	}
}

// 同样租约时长的容器，不管什么时候续约过，下一次续约都落在同一个时刻。
func TestNextRenewAligned(t *testing.T) {
	L := 30 * time.Second
	base := time.Unix(1_800_000_000, 0)
	a := nextRenewAt(base.Add(1*time.Second), L)
	b := nextRenewAt(base.Add(7*time.Second), L)
	if !a.Equal(b) {
		t.Fatalf("没有对齐：%v %v", a, b)
	}
	if d := a.Sub(base.Add(1 * time.Second)); d <= 0 || d > L/3 {
		t.Fatalf("续约间隔 %v 超过了 1/3 租约", d)
	}
}
