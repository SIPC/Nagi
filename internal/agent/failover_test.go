package agent

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// v0.35.0#23：回复里「更高的轮次」的持有方不是参与者（A 或 B）时不算，demote 也再核对一次：
// 被攻破的见证人不能让持有方撤掉容器、把任意公钥登记成副本的源节点。
func TestFailoverIgnoresNonParticipantHolder(t *testing.T) {
	key := func(b byte) []byte { k := make([]byte, 32); k[0] = b; return k }
	A, B, W, X := key(1), key(2), key(3), key(4)
	cfg := &nagiv1.FailoverConfig{ProjectId: "0123456789abcdef", Container: "db",
		A: &nagiv1.FailoverParticipant{Key: A, Name: "a"}, B: &nagiv1.FailoverParticipant{Key: B, Name: "b"}, Witness: &nagiv1.FailoverParticipant{Key: W, Name: "w"}}
	var witnessHolder []byte
	a := &agent{log: slog.New(slog.DiscardHandler)}
	a.votes = &voteBatcher{send: func(_ context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i := range reqs {
			if k[0] == W[0] {
				out[i] = &nagiv1.LeaseVoteResponse{Epoch: 100, Holder: witnessHolder}
			} else {
				out[i] = &nagiv1.LeaseVoteResponse{} // B 联系不上一样，不投票
			}
		}
		return out, nil
	}}
	others := []*nagiv1.FailoverParticipant{cfg.GetB(), cfg.GetWitness()}
	for _, h := range [][]byte{X, W, nil} {
		witnessHolder = h
		if r := a.collect(context.Background(), others, cfg, 5); r.higher != nil {
			t.Fatalf("持有方 %x 不是参与者，不应该算更高的轮次", h[:min(len(h), 1)])
		}
	}
	witnessHolder = B
	if r := a.collect(context.Background(), others, cfg, 5); r.higher == nil || r.higher.GetEpoch() != 100 {
		t.Fatal("持有方是另一个参与者时应该算更高的轮次")
	}

	// demote 再核对一次：持有方不是参与者时什么都不改。
	id := unitOf(cfg)
	st := &failoverState{cfg: cfg, holder: A, epoch: 5}
	a.failovers = &failovers{m: map[string]*failoverState{id: st}}
	a.demote(context.Background(), id, X, 100, true)
	if !bytes.Equal(st.holder, A) || st.epoch != 5 {
		t.Fatalf("不应该转为 %x 的热备", st.holder[:1])
	}
	if isParticipant(cfg, W) || isParticipant(cfg, nil) || !isParticipant(cfg, A) || !isParticipant(cfg, B) {
		t.Fatal("isParticipant 只认 A 和 B")
	}
}

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
