package agent

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/lease"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 投票人都记着「票投给了本节点、轮次比本节点记着的高」（例如本节点调高轮次以后、记下新轮次之前重启了）：
// 持有者把轮次调到它之上再续约，能续上；这期间另一方要不到票，不会两边同时认为自己是持有者。
func TestHolderRaisesEpochForOwnHigherVote(t *testing.T) {
	key := func(b byte) []byte { k := make([]byte, 32); k[0] = b; return k }
	A, B, W := key(1), key(2), key(3)
	cfg := &nagiv1.FailoverConfig{ProjectId: "0123456789abcdef", Container: "db", LeaseSeconds: 9,
		A: &nagiv1.FailoverParticipant{Key: A, Name: "a"}, B: &nagiv1.FailoverParticipant{Key: B, Name: "b"}, Witness: &nagiv1.FailoverParticipant{Key: W, Name: "w"}}
	id, L := unitOf(cfg), leaseOf(cfg)
	dir := t.TempDir()
	vA := lease.NewVoter(filepath.Join(dir, "a"))
	vB := lease.NewVoter(filepath.Join(dir, "b"))
	vW := lease.NewVoter(filepath.Join(dir, "w"))
	for _, v := range []*lease.Voter{vA, vB, vW} {
		if err := v.Set(id, A, 7, L); err != nil {
			t.Fatal(err)
		}
	}
	a := &agent{log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	a.votes = &voteBatcher{send: func(_ context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		v := vB
		if k[0] == W[0] {
			v = vW
		}
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i, r := range reqs {
			granted, holder, epoch := v.Vote(id, participants(cfg), A, r.GetEpoch(), L)
			out[i] = &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
		}
		return out, nil
	}}
	// restoring：这里只看续约，不碰容器。
	st := &failoverState{cfg: cfg, holder: A, epoch: 5, restoring: true}
	a.failovers = &failovers{dir: filepath.Join(dir, "failover"), voter: vA, m: map[string]*failoverState{id: st}}
	ctx := context.Background()

	// 第一次续约：三方都不同意（记着 A 的第 7 轮），本节点把轮次调到 8。
	a.holderTick(ctx, id, st, cfg, cfg.GetA(), cfg.GetB(), 5)
	if st.epoch != 8 || !bytes.Equal(st.holder, A) {
		t.Fatalf("应该把轮次调到 8：holder=%x epoch=%d", st.holder[:1], st.epoch)
	}
	if !st.leaseEnd.IsZero() {
		t.Fatal("第一次续约没拿到票，不应该算续上")
	}
	// 用新的轮次再续一次：都同意。
	a.holderTick(ctx, id, st, cfg, cfg.GetA(), cfg.GetB(), st.epoch)
	if !time.Now().Before(st.leaseEnd) {
		t.Fatal("调高轮次以后应该续上租约")
	}
	for name, v := range map[string]*lease.Voter{"A": vA, "B": vB, "W": vW} {
		if h, e, expired := v.State(id, L); !bytes.Equal(h, A) || e != 8 || expired {
			t.Fatalf("%s 的票应该是 A 的第 8 轮：%x %d %v", name, h[:1], e, expired)
		}
	}
	// 这期间另一方 B 要第 9 轮的票，哪一方都不给。
	for name, v := range map[string]*lease.Voter{"A": vA, "B": vB, "W": vW} {
		if ok, _, _ := v.Vote(id, participants(cfg), B, 9, L); ok {
			t.Fatalf("A 持有租约时 %s 不应该投给 B", name)
		}
	}
}

func TestRaisedEpoch(t *testing.T) {
	other := &nagiv1.LeaseVoteResponse{Epoch: 9, Holder: []byte("b")}
	for _, c := range []struct {
		name string
		r    voteResult
		ok   bool
		want uint64
	}{
		{"没有更高的轮次", voteResult{}, true, 5},
		{"另一方更高、本节点拿到两票", voteResult{higher: other}, true, 10},
		{"另一方更高、本节点没拿到两票：被顶替了，不调", voteResult{higher: other}, false, 5},
		{"投给本节点的更高轮次、没拿到两票", voteResult{selfHigher: 7}, false, 8},
		{"投给本节点的更高轮次、拿到两票", voteResult{selfHigher: 7}, true, 8},
		{"两种都有、拿到两票", voteResult{higher: other, selfHigher: 12}, true, 13},
		{"两种都有、没拿到两票：被顶替了，不调", voteResult{higher: other, selfHigher: 12}, false, 5},
		{"轮次到顶不调", voteResult{selfHigher: math.MaxUint64}, false, 5},
	} {
		if got := raisedEpoch(c.r, c.ok, 5); got != c.want {
			t.Errorf("%s：%d，应为 %d", c.name, got, c.want)
		}
	}
}
