package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/logging"
	"github.com/SIPC/nagi/internal/pkg/lease"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestHolderRecoversAfterWitnessCreatesEpochGap(t *testing.T) {
	key := func(b byte) []byte { k := make([]byte, 32); k[0] = b; return k }
	A, B, W := key(1), key(2), key(3)
	cfg := &nagiv1.FailoverConfig{ProjectId: "0123456789abcdef", Container: "db", LeaseSeconds: 9,
		A: &nagiv1.FailoverParticipant{Key: A}, B: &nagiv1.FailoverParticipant{Key: B}, Witness: &nagiv1.FailoverParticipant{Key: W}}
	id, L := unitOf(cfg), leaseOf(cfg)
	dir := t.TempDir()
	vA, vB := lease.NewVoter(filepath.Join(dir, "a")), lease.NewVoter(filepath.Join(dir, "b"))
	for _, v := range []*lease.Voter{vA, vB} {
		if err := v.Set(id, A, 1, L); err != nil {
			t.Fatal(err)
		}
	}
	bOnline := false
	a := &agent{log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	a.votes = &voteBatcher{send: func(_ context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		if (k[0] == B[0]) != bOnline {
			return nil, errors.New("offline")
		}
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i, r := range reqs {
			if !bOnline {
				out[i] = &nagiv1.LeaseVoteResponse{Holder: A, Epoch: r.GetEpoch() + 1023}
			} else {
				ok, h, e := vB.Vote(id, participants(cfg), A, r.GetEpoch(), L)
				out[i] = &nagiv1.LeaseVoteResponse{Granted: ok, Holder: h, Epoch: e}
			}
		}
		return out, nil
	}}
	st := &failoverState{cfg: cfg, holder: A, epoch: 1, restoring: true}
	a.failovers = &failovers{dir: filepath.Join(dir, "failover"), voter: vA, m: map[string]*failoverState{id: st}}
	ctx := context.Background()
	for range 2 {
		st.renewed = false
		a.holderTick(ctx, id, st, cfg, cfg.GetA(), cfg.GetB(), st.epoch)
	}
	if st.epoch != 2049 || !st.leaseEnd.IsZero() {
		t.Fatalf("expected unleased epoch gap: epoch=%d lease=%v", st.epoch, st.leaseEnd)
	}
	// B 恢复、恶意见证人离线：不改设置、不重置票，两个诚实节点自行恢复多数票。
	bOnline = true
	for range 3 {
		st.renewed = false
		a.holderTick(ctx, id, st, cfg, cfg.GetA(), cfg.GetB(), st.epoch)
	}
	if !time.Now().Before(st.leaseEnd) || st.epoch != 2049 {
		t.Fatalf("honest nodes failed to renew: epoch=%d lease=%v", st.epoch, st.leaseEnd)
	}
	if h, e, expired := vB.State(id, L); !bytes.Equal(h, A) || e != 2049 || expired {
		t.Fatalf("B failed to catch up: holder=%x epoch=%d expired=%v", h, e, expired)
	}
	// A 随后离线，W 保留第 4097 轮；B 从 2049 分段追赶，提示不能直接成为租约。
	if err := vB.Set(id, A, 2049, -time.Second); err != nil {
		t.Fatal(err)
	}
	vW := lease.NewVoter(filepath.Join(dir, "w"))
	if err := vW.Set(id, A, 4097, -time.Second); err != nil {
		t.Fatal(err)
	}
	standby := &agent{log: slog.New(slog.DiscardHandler), replicas: newReplicas(filepath.Join(dir, "standby"))}
	if err := standby.replicas.save(&replicaMeta{ProjectID: cfg.ProjectId, Container: cfg.Container, SourceKey: A}, &nagiv1.ContainerSpec{ProjectId: cfg.ProjectId, Name: cfg.Container}); err != nil {
		t.Fatal(err)
	}
	bst := &failoverState{cfg: cfg, holder: A, epoch: 2049}
	standby.failovers = &failovers{dir: filepath.Join(dir, "b-failover"), voter: vB, m: map[string]*failoverState{id: bst}}
	standby.votes = &voteBatcher{send: func(_ context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		if k[0] != W[0] {
			return nil, errors.New("offline")
		}
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i, req := range reqs {
			ok, h, e := vW.Vote(id, participants(cfg), B, req.GetEpoch(), L)
			out[i] = &nagiv1.LeaseVoteResponse{Granted: ok, Holder: h, Epoch: e}
		}
		return out, nil
	}}
	for _, want := range []uint64{3073, 4097} {
		standby.standbyTick(ctx, id, bst, cfg, cfg.GetB(), cfg.GetA())
		h, e, expired := vB.State(id, L)
		if !bytes.Equal(h, A) || e != want || !expired || !bst.leaseEnd.IsZero() || !bytes.Equal(bst.holder, A) {
			t.Fatalf("catch-up hint became lease: holder=%x epoch=%d expired=%v", h, e, expired)
		}
	}
	r := standby.collect(ctx, B, []*nagiv1.FailoverParticipant{cfg.GetWitness(), cfg.GetA()}, cfg, 4098)
	if ok, _, _ := vB.Vote(id, participants(cfg), B, 4098, L); r.granted != 1 || !ok {
		t.Fatal("caught-up standby cannot acquire majority after A is gone")
	}
}

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
	var hostileHolder []byte
	a.votes = &voteBatcher{send: func(_ context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
		v := vB
		if k[0] == W[0] {
			v = vW
		}
		out := make([]*nagiv1.LeaseVoteResponse, len(reqs))
		for i, r := range reqs {
			if k[0] == W[0] && hostileHolder != nil {
				out[i] = &nagiv1.LeaseVoteResponse{Holder: hostileHolder, Epoch: math.MaxUint64 - 1}
				continue
			}
			granted, holder, epoch := v.Vote(id, participants(cfg), A, r.GetEpoch(), L)
			out[i] = &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
		}
		return out, nil
	}}
	// restoring：这里只看续约，不碰容器。
	st := &failoverState{cfg: cfg, holder: A, epoch: 5, restoring: true}
	a.failovers = &failovers{dir: filepath.Join(dir, "failover"), voter: vA, m: map[string]*failoverState{id: st}}
	ctx := context.Background()
	logs, err := logstore.Open(ctx, filepath.Join(dir, "logs.db"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a.failovers.history = logging.NewDecisions(logs, 64)
	defer a.failovers.history.Close()

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
	// 一名见证人不能把仍有多数票的持有者和诚实投票者推进到整数边界。
	for _, holder := range [][]byte{A, B} {
		hostileHolder = holder
		st.renewed = false
		a.holderTick(ctx, id, st, cfg, cfg.GetA(), cfg.GetB(), st.epoch)
		if st.epoch != 8 || !time.Now().Before(st.leaseEnd) {
			t.Fatalf("hostile witness poisoned holder: epoch=%d", st.epoch)
		}
		_, epoch, _ := vB.State(id, L)
		if epoch != 8 {
			t.Fatalf("honest voter poisoned: %d", epoch)
		}
	}
	a.failovers.history.Close()
	rows, err := logs.Query(ctx, logstore.Filter{Category: logstore.Task})
	if err != nil {
		t.Fatal(err)
	}
	var renewals []struct {
		Votes   int
		Renewed bool
		Epoch   uint64 `json:"requested_epoch"`
	}
	for _, row := range rows {
		if row.Kind != "failover.renewal" {
			continue
		}
		var data struct {
			Votes   int
			Renewed bool
			Epoch   uint64 `json:"requested_epoch"`
		}
		if err := json.Unmarshal(row.Data.(json.RawMessage), &data); err != nil {
			t.Fatal(err)
		}
		renewals = append(renewals, data)
	}
	if len(renewals) != 4 || renewals[0].Renewed || renewals[0].Votes != 0 || renewals[0].Epoch != 5 || !renewals[1].Renewed || renewals[1].Votes != 3 || renewals[1].Epoch != 8 {
		t.Fatalf("renewal evidence: %+v", renewals)
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
		{"见证人报告极大轮次", voteResult{selfHigher: math.MaxUint64 - 1}, false, 5},
		{"另一方报告极大轮次", voteResult{higher: &nagiv1.LeaseVoteResponse{Epoch: math.MaxUint64 - 1}}, true, 5},
	} {
		if got := raisedEpoch(c.r, c.ok, 5); got != c.want {
			t.Errorf("%s：%d，应为 %d", c.name, got, c.want)
		}
	}
}
