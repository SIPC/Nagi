package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/lease"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 顶替（见 docs/failover.md），按容器。每个设置了顶替的容器有 A、B 两个能运行它的节点和一个见证人，
// 三方各一票，拿到 2 票（租约）的一方运行容器：
//   - 持有者每 1/3 个租约时长续约一次。续不上时，在"租约到期前 5 秒 + 停止超时"开始停止容器，
//     到期前 5 秒还没停完就强制结束；之后又续上了就重新启动。
//   - 热备平时接收持有者的同步、提前拉好镜像。它投给持有者的票作废（超过租约时长没收到续约）后，
//     先向见证人和原持有者要票，拿到一票再投自己，凑够 2 票就成为新的持有者（轮次加一），
//     再等 5 秒，用副本恢复并启动容器（配置是面板登记副本时给的）。有状态容器默认先告警，等用户确认。
//   - 原持有者回来后续约被拒绝，看到更高的轮次，就停掉容器、把目录改名保留，转为新持有者的热备。
//   - 自动切回（auto_failback）：容器不在原节点 A 上运行、而 A 已经连续稳定 failbackAfter 时，持有者有计划地
//     把容器交还给 A：同步一次、停止容器、再同步最后的数据，然后把租约转让给 A。A 发现自己成了持有者、
//     手上只有副本，就用副本恢复并启动。和手动迁移一样不丢数据。
// 每个容器的票、设置都以"项目 ID-容器名"为键。

const (
	defaultLease = 30 * time.Second
	stopTimeout  = 10 * time.Second // compose 默认给容器的优雅退出时间
	safetyMargin = 5 * time.Second
	syncEvery    = 15 * time.Minute
	startupGrace = 10 * time.Second
	// failbackAfter：原节点连续这么久每次续约都投了票，才自动切回，避免节点时好时坏时来回切。
	failbackAfter = 10 * time.Minute
)

type failoverFile struct {
	Config json.RawMessage `json:"config"`
	Holder []byte          `json:"holder"`
	Epoch  uint64          `json:"epoch"`
	// 最近一次同步给热备（持有者）或收到同步（热备）的时间，重启后界面上照常显示。
	LastSync int64 `json:"last_sync,omitempty"`
}

type failoverState struct {
	cfg    *nagiv1.FailoverConfig
	holder []byte
	epoch  uint64

	leaseEnd   time.Time
	nextRenew  time.Time
	renewed    bool // 本次启动后续约过（至少尝试过）
	fenced     bool
	restoring  bool
	everOK     bool // 本次启动后续约成功过
	supervised time.Time
	pending    bool // 等待用户确认顶替
	confirmed  bool
	lastSync   time.Time
	nextSync   time.Time
	lastError  string
	lastPull   time.Time
	cancelLoop context.CancelFunc
	// 自动切回：原节点从什么时候起每次续约都投了票（零值表示现在不稳定），以及是否正在切回。
	preferredSince time.Time
	failingBack    bool
}

type failovers struct {
	dir   string
	voter *lease.Voter
	mu    sync.Mutex
	m     map[string]*failoverState // 项目 ID-容器名
}

func newFailovers(dataDir string) *failovers {
	dir := filepath.Join(dataDir, "failover")
	f := &failovers{dir: dir, voter: lease.NewVoter(filepath.Join(dir, "votes")), m: map[string]*failoverState{}}
	f.loadAll()
	return f
}

func unitOf(cfg *nagiv1.FailoverConfig) string { return cfg.GetProjectId() + "-" + cfg.GetContainer() }

func leaseOf(cfg *nagiv1.FailoverConfig) time.Duration {
	if s := cfg.GetLeaseSeconds(); s >= 9 {
		return time.Duration(s) * time.Second
	}
	return defaultLease
}

func participants(cfg *nagiv1.FailoverConfig) [][]byte {
	return [][]byte{cfg.GetA().GetKey(), cfg.GetB().GetKey()}
}

func (f *failovers) file(id string) string { return filepath.Join(f.dir, id+".json") }

// save 保存设置和已知的持有者。调用方持有 f.mu。
func (f *failovers) save(id string, st *failoverState) error {
	cfg, err := protojson.Marshal(st.cfg)
	if err != nil {
		return err
	}
	ff := failoverFile{Config: cfg, Holder: st.holder, Epoch: st.epoch}
	if !st.lastSync.IsZero() {
		ff.LastSync = st.lastSync.Unix()
	}
	b, _ := json.Marshal(ff)
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(f.file(id), b)
}

func (f *failovers) loadAll() {
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			continue
		}
		var ff failoverFile
		cfg := &nagiv1.FailoverConfig{}
		if json.Unmarshal(b, &ff) != nil || protojson.Unmarshal(ff.Config, cfg) != nil || unitOf(cfg) != id {
			continue
		}
		st := &failoverState{cfg: cfg, holder: ff.Holder, epoch: ff.Epoch}
		if ff.LastSync > 0 {
			st.lastSync = time.Unix(ff.LastSync, 0)
		}
		f.m[id] = st
	}
}

func (f *failovers) get(id string) *failoverState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[id]
}

func (f *failovers) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.m))
	for id := range f.m {
		out = append(out, id)
	}
	return out
}

// ---- 角色 ----

func (a *agent) selfKey() []byte {
	if a.mesh == nil {
		return nil
	}
	k := a.mesh.m.PublicKey()
	return k[:]
}

// selfParticipant 返回本节点在设置里是 A 还是 B（是见证人时返回 nil），以及另一个节点。
func (a *agent) selfParticipant(cfg *nagiv1.FailoverConfig) (me, other *nagiv1.FailoverParticipant) {
	k := a.selfKey()
	switch {
	case bytes.Equal(cfg.GetA().GetKey(), k):
		return cfg.GetA(), cfg.GetB()
	case bytes.Equal(cfg.GetB().GetKey(), k):
		return cfg.GetB(), cfg.GetA()
	}
	return nil, nil
}

func participantName(cfg *nagiv1.FailoverConfig, key []byte) string {
	for _, m := range []*nagiv1.FailoverParticipant{cfg.GetA(), cfg.GetB(), cfg.GetWitness()} {
		if bytes.Equal(m.GetKey(), key) {
			return m.GetName()
		}
	}
	return ""
}

// ---- 投票（MeshService） ----

// maxBatchVotes 是一次请求里最多的投票数。
const maxBatchVotes = 1024

func (s *meshService) LeaseVotes(ctx context.Context, req *nagiv1.LeaseVotesRequest) (*nagiv1.LeaseVotesResponse, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	if len(req.GetVotes()) > maxBatchVotes {
		return nil, status.Error(codes.InvalidArgument, "一次投票请求太多")
	}
	out := &nagiv1.LeaseVotesResponse{}
	for _, v := range req.GetVotes() {
		out.Votes = append(out.Votes, s.a.leaseVote(caller, v))
	}
	return out, nil
}

// leaseVote 处理一个容器的投票请求。本节点没有这个容器的顶替设置时不同意。
func (a *agent) leaseVote(caller mesh.Key, req *nagiv1.LeaseVoteRequest) *nagiv1.LeaseVoteResponse {
	id, err := replicaID(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return &nagiv1.LeaseVoteResponse{}
	}
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return &nagiv1.LeaseVoteResponse{}
	}
	f.mu.Lock()
	cfg := st.cfg
	f.mu.Unlock()
	if to := req.GetHandoverTo(); len(to) > 0 {
		granted, holder, epoch := f.voter.Handover(id, participants(cfg), caller[:], to, req.GetEpoch(), leaseOf(cfg))
		return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
	}
	granted, holder, epoch := f.voter.Vote(id, participants(cfg), caller[:], req.GetEpoch(), leaseOf(cfg))
	return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
}

// requestVote 向另一个参与者要票；handoverTo 不为空时是请它把票转让给这一方。
// 发给同一个成员的投票由 a.votes 合并成一次请求。
func (a *agent) requestVote(ctx context.Context, m *nagiv1.FailoverParticipant, cfg *nagiv1.FailoverConfig, epoch uint64, handoverTo ...[]byte) (*nagiv1.LeaseVoteResponse, error) {
	k, err := mesh.ParseKey(m.GetKey())
	if err != nil {
		return nil, err
	}
	req := &nagiv1.LeaseVoteRequest{ProjectId: cfg.GetProjectId(), Container: cfg.GetContainer(), Epoch: epoch}
	if len(handoverTo) > 0 {
		req.HandoverTo = handoverTo[0]
	}
	return a.votes.vote(ctx, k, req)
}

// voteBatcher 把短时间内发给同一个成员的投票合并成一次 LeaseVotes 请求。
// 同样租约时长的容器在同一秒续约（见 nextRenewAt），所以平时一对节点之间每次续约只有一次请求。
type voteBatcher struct {
	a *agent
	// send 发出一次合并的请求；为空时经 mesh 发给成员 k。
	send    func(ctx context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error)
	mu      sync.Mutex
	pending map[mesh.Key]*voteBatch
}

type voteBatch struct {
	reqs  []*nagiv1.LeaseVoteRequest
	resps []*nagiv1.LeaseVoteResponse
	err   error
	done  chan struct{}
}

// batchWindow 是等其他投票一起发出的时间。
const batchWindow = 30 * time.Millisecond

func (b *voteBatcher) vote(ctx context.Context, k mesh.Key, req *nagiv1.LeaseVoteRequest) (*nagiv1.LeaseVoteResponse, error) {
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[mesh.Key]*voteBatch{}
	}
	bt := b.pending[k]
	if bt == nil || len(bt.reqs) >= maxBatchVotes {
		bt = &voteBatch{done: make(chan struct{})}
		b.pending[k] = bt
		time.AfterFunc(batchWindow, func() { b.flush(k, bt) })
	}
	i := len(bt.reqs)
	bt.reqs = append(bt.reqs, req)
	b.mu.Unlock()
	select {
	case <-bt.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if bt.err != nil {
		return nil, bt.err
	}
	if i >= len(bt.resps) {
		return nil, errors.New("投票结果不完整")
	}
	return bt.resps[i], nil
}

func (b *voteBatcher) flush(k mesh.Key, bt *voteBatch) {
	b.mu.Lock()
	if b.pending[k] == bt {
		delete(b.pending, k)
	}
	reqs := bt.reqs
	b.mu.Unlock()
	defer close(bt.done)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	send := b.send
	if send == nil {
		send = b.sendMesh
	}
	bt.resps, bt.err = send(ctx, k, reqs)
}

func (b *voteBatcher) sendMesh(ctx context.Context, k mesh.Key, reqs []*nagiv1.LeaseVoteRequest) ([]*nagiv1.LeaseVoteResponse, error) {
	cc, err := mesh.Client(b.a.mesh.m, k)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	resp, err := nagiv1.NewMeshServiceClient(cc).LeaseVotes(ctx, &nagiv1.LeaseVotesRequest{Votes: reqs})
	if err != nil {
		return nil, err
	}
	return resp.GetVotes(), nil
}

// nextRenewAt 返回下一次续约的时间：对齐到 1/3 租约时长的整数倍（按 Unix 时间），
// 同样租约时长的容器在同一秒续约，发给同一个成员的投票就能合并。
func nextRenewAt(now time.Time, L time.Duration) time.Time {
	step := L / 3
	return now.Truncate(step).Add(step)
}

type voteResult struct {
	granted   int
	grantedBy [][]byte                  // 投了票的各方的公钥
	higher    *nagiv1.LeaseVoteResponse // 某一方告诉我们的更高轮次
}

// collect 并行向 others 要票。
func (a *agent) collect(ctx context.Context, others []*nagiv1.FailoverParticipant, cfg *nagiv1.FailoverConfig, epoch uint64) voteResult {
	var r voteResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range others {
		wg.Add(1)
		go func(m *nagiv1.FailoverParticipant) {
			defer wg.Done()
			resp, err := a.requestVote(ctx, m, cfg, epoch)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if resp.GetGranted() {
				r.granted++
				r.grantedBy = append(r.grantedBy, m.GetKey())
			} else if resp.GetEpoch() > epoch && (r.higher == nil || resp.GetEpoch() > r.higher.GetEpoch()) {
				r.higher = resp
			}
		}(m)
	}
	wg.Wait()
	return r
}

// ---- 循环 ----

// startFailover 启动一个容器的顶替循环（本节点是 A 或 B 时）。
func (a *agent) startFailover(ctx context.Context, id string) {
	f := a.failovers
	f.mu.Lock()
	st := f.m[id]
	if st == nil || st.cancelLoop != nil {
		f.mu.Unlock()
		return
	}
	if me, _ := a.selfParticipant(st.cfg); me == nil {
		f.mu.Unlock()
		return // 见证人只投票
	}
	lctx, cancel := context.WithCancel(ctx)
	st.cancelLoop = cancel
	f.mu.Unlock()
	go func() {
		for {
			a.failoverTick(lctx, id)
			// 各个容器的循环都在整秒运行，同一秒到期的续约才能合并成一次请求。
			select {
			case <-lctx.Done():
				return
			case <-time.After(time.Until(time.Now().Truncate(time.Second).Add(time.Second))):
			}
		}
	}()
}

func (a *agent) failoverTick(ctx context.Context, id string) {
	f := a.failovers
	st := f.get(id)
	if st == nil || a.mesh == nil {
		return
	}
	f.mu.Lock()
	cfg, holder, epoch := st.cfg, st.holder, st.epoch
	f.mu.Unlock()
	me, other := a.selfParticipant(cfg)
	if me == nil {
		return
	}
	if bytes.Equal(holder, me.GetKey()) {
		a.holderTick(ctx, id, st, cfg, me, other, epoch)
	} else {
		a.standbyTick(ctx, id, st, cfg, me, other)
	}
}

func (a *agent) holderTick(ctx context.Context, id string, st *failoverState, cfg *nagiv1.FailoverConfig, me, other *nagiv1.FailoverParticipant, epoch uint64) {
	f := a.failovers
	pid, name := cfg.GetProjectId(), cfg.GetContainer()
	L := leaseOf(cfg)
	now := time.Now()
	f.mu.Lock()
	due := !st.renewed || !now.Before(st.nextRenew)
	f.mu.Unlock()
	if due {
		sent := time.Now()
		r := a.collect(ctx, []*nagiv1.FailoverParticipant{other, cfg.GetWitness()}, cfg, epoch)
		self, _, _ := f.voter.Vote(id, participants(cfg), me.GetKey(), epoch, L)
		votes := r.granted
		if self {
			votes++
		}
		f.mu.Lock()
		st.renewed = true
		st.nextRenew = nextRenewAt(now, L)
		ok := votes >= 2
		if ok {
			st.leaseEnd = sent.Add(L)
			st.everOK = true
		} else {
			st.nextRenew = now.Add(2 * time.Second) // 续不上时尽快重试
		}
		wasFenced := st.fenced
		// 自动切回：记下原节点（A）从什么时候起一直在投票。
		startFailback := false
		if cfg.GetAutoFailback() && !bytes.Equal(me.GetKey(), cfg.GetA().GetKey()) {
			if slices.ContainsFunc(r.grantedBy, func(k []byte) bool { return bytes.Equal(k, cfg.GetA().GetKey()) }) {
				if st.preferredSince.IsZero() {
					st.preferredSince = now
				}
			} else {
				st.preferredSince = time.Time{}
			}
			startFailback = ok && !st.failingBack && !st.fenced && !st.restoring && !st.preferredSince.IsZero() &&
				now.Sub(st.preferredSince) >= failbackAfter
			if startFailback {
				st.failingBack = true
			}
		}
		f.mu.Unlock()
		if startFailback && a.projects.Has(ctx, pid, name) {
			go a.failback(context.WithoutCancel(ctx), id)
		} else if startFailback {
			f.mu.Lock()
			st.failingBack = false
			f.mu.Unlock()
		}
		if !ok && r.higher != nil {
			a.demote(ctx, id, r.higher.GetHolder(), r.higher.GetEpoch(), true)
			return
		}
		if ok && wasFenced && a.projects.Has(ctx, pid, name) {
			a.log.Info("重新拿到租约，启动容器", "project", pid, "container", name)
			if err := a.projects.Unfence(ctx, pid, name, func(string) {}); err == nil {
				f.mu.Lock()
				st.fenced = false
				f.mu.Unlock()
				a.alerts.Resolve(ctx, "lease:"+id)
				a.changed()
			}
		}
	}

	f.mu.Lock()
	leaseEnd, fenced, restoring, everOK, failingBack := st.leaseEnd, st.fenced, st.restoring, st.everOK, st.failingBack
	superviseDue := time.Since(st.supervised) >= 2*time.Second
	if superviseDue {
		st.supervised = time.Now()
	}
	f.mu.Unlock()
	// 刚启动时还要等 WireGuard 握手，头 10 秒内续不上先不停（Docker 不会自己拉起顶替容器，这段时间它本来就没在运行）。
	grace := !everOK && time.Since(a.startedAt) < startupGrace
	if !fenced && !restoring && !failingBack && !grace && time.Now().After(leaseEnd.Add(-safetyMargin-stopTimeout)) && a.projects.Has(ctx, pid, name) {
		a.log.Warn("续不上租约，停止容器", "project", pid, "container", name)
		a.alerts.Fire(ctx, "lease:"+id, "容器 "+name+" 续不上顶替租约，已停止", "联系不上另一个节点和见证人；恢复后会自动重新启动", pid)
		_ = a.projects.Fence(ctx, pid, name, leaseEnd.Add(-safetyMargin), func(string) {})
		f.mu.Lock()
		st.fenced = true
		f.mu.Unlock()
		// 下面的代管重启不能用停止前读到的状态，否则会把刚停掉的容器又拉起来。
		fenced = true
		a.changed()
	}

	// 持有租约时，按用户原来的重启策略代为重启退出的容器；机器重启后也由这里把容器启动起来。
	if superviseDue && !fenced && !restoring && !failingBack && everOK && time.Now().Before(leaseEnd) && a.projects.Has(ctx, pid, name) {
		a.projects.Supervise(ctx, pid, name)
	}

	// 成了持有者、手上却只有副本：对方把容器交还给了本节点（自动切回），用副本恢复并启动。
	if everOK && !fenced && !restoring && !failingBack && !a.projects.Has(ctx, pid, name) {
		if _, _, _, err := a.replicas.load(pid, name); err == nil {
			f.mu.Lock()
			st.restoring = true
			f.mu.Unlock()
			a.startFromReplica(ctx, id, st, me, 0, "容器已切回本节点", "原节点恢复后，容器 "+name+" 从 "+other.GetName()+" 切回了本节点")
		}
	}

	// 定时同步给热备（另外每次定时快照后也会同步）。
	f.mu.Lock()
	// 只有持有者同步给热备（刚被顶替、正在退位的节点不能再同步）。
	syncDue := !st.fenced && !st.restoring && !st.failingBack && bytes.Equal(st.holder, a.selfKey()) && time.Now().After(st.nextSync)
	if syncDue {
		st.nextSync = time.Now().Add(syncEvery)
	}
	f.mu.Unlock()
	if syncDue && a.projects.Has(ctx, pid, name) {
		go a.syncStandby(context.WithoutCancel(ctx), id)
	}
}

// syncStandby 持有者把副本同步给热备。不是持有者时什么都不做。
func (a *agent) syncStandby(ctx context.Context, id string) {
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return
	}
	f.mu.Lock()
	cfg := st.cfg
	isHolder := bytes.Equal(st.holder, a.selfKey()) && !st.fenced && !st.restoring && !st.failingBack
	f.mu.Unlock()
	me, other := a.selfParticipant(cfg)
	if me == nil || !isHolder {
		return
	}
	_, err := a.syncTo(ctx, cfg.GetProjectId(), cfg.GetContainer(), &nagiv1.BackupTarget{NodeId: other.GetNodeId(), Name: other.GetName(), Key: other.GetKey()})
	f.mu.Lock()
	if err != nil && !bytes.Equal(st.holder, a.selfKey()) {
		// 同步的过程中本节点被顶替、退为热备了：对方拒绝是正常的，不算失败。
		f.mu.Unlock()
		return
	}
	if err != nil {
		st.lastError = "同步给热备失败：" + err.Error()
		st.nextSync = time.Now().Add(time.Minute) // 热备可能暂时不在，1 分钟后重试
	} else {
		st.lastError, st.lastSync = "", time.Now()
		_ = f.save(id, st)
	}
	f.mu.Unlock()
	a.changed()
	if err != nil {
		a.alerts.Fire(ctx, "standby:"+id, "容器 "+cfg.GetContainer()+" 同步给热备节点失败", err.Error(), cfg.GetProjectId())
	} else {
		a.alerts.Resolve(ctx, "standby:"+id)
	}
}

func (a *agent) standbyTick(ctx context.Context, id string, st *failoverState, cfg *nagiv1.FailoverConfig, me, other *nagiv1.FailoverParticipant) {
	f := a.failovers
	pid, name := cfg.GetProjectId(), cfg.GetContainer()
	L := leaseOf(cfg)
	holder, epoch, expired := f.voter.State(id, L)
	f.mu.Lock()
	if epoch > st.epoch {
		st.holder, st.epoch = holder, epoch
		_ = f.save(id, st)
	}
	if !expired {
		wasPending := st.pending
		st.pending, st.confirmed = false, false
		f.mu.Unlock()
		if wasPending {
			a.changed()
		}
		a.alerts.Resolve(ctx, "takeover:"+id)
		return
	}
	confirmed, pending := st.confirmed, st.pending
	f.mu.Unlock()

	// 持有者失联。
	_, sp, _, err := a.replicas.load(pid, name)
	if err != nil {
		f.mu.Lock()
		st.lastError = "持有者失联，但热备还没有登记副本，无法顶替"
		f.mu.Unlock()
		a.alerts.Fire(ctx, "takeover:"+id, "容器 "+name+" 无法顶替", "热备还没有登记副本", pid)
		return
	}
	if sp.GetStateful() && !cfg.GetAutoStateful() && !confirmed {
		if !pending {
			f.mu.Lock()
			st.pending = true
			f.mu.Unlock()
			a.changed()
			a.log.Warn("持有者失联，有状态容器等待确认顶替", "project", pid, "container", name)
			a.alerts.Fire(ctx, "takeover:"+id, "容器 "+name+" 所在节点失联，等待确认顶替",
				"有状态容器：顶替会丢失最后一次同步之后写入的数据。请在面板上确认", pid)
		}
		return
	}

	// 先向见证人和原持有者要票，拿到一票再投自己。
	newEpoch := epoch + 1
	sent := time.Now()
	r := a.collect(ctx, []*nagiv1.FailoverParticipant{cfg.GetWitness(), other}, cfg, newEpoch)
	if r.granted == 0 {
		return
	}
	if ok, _, _ := f.voter.Vote(id, participants(cfg), me.GetKey(), newEpoch, L); !ok {
		return
	}
	a.log.Warn("拿到多数票，顶替容器", "project", pid, "container", name, "epoch", newEpoch)
	f.mu.Lock()
	st.holder, st.epoch = me.GetKey(), newEpoch
	st.leaseEnd = sent.Add(L)
	st.nextRenew = nextRenewAt(time.Now(), L)
	st.renewed = true
	st.everOK = true
	st.restoring = true
	st.pending, st.confirmed = false, false
	_ = f.save(id, st)
	f.mu.Unlock()
	a.alerts.Resolve(ctx, "takeover:"+id)

	a.startFromReplica(ctx, id, st, me, safetyMargin, "已顶替容器", "原节点 "+participantName(cfg, holder)+" 失联，容器 "+name+" 已在本节点启动")
}

// startFromReplica 成为持有者以后在后台用副本恢复并启动容器（顶替和切回共用），循环照常续约。
// wait 是开始前先等的时间（顶替时等原持有者的租约彻底过期）。调用方已经把 st.restoring 设为 true。
func (a *agent) startFromReplica(ctx context.Context, id string, st *failoverState, me *nagiv1.FailoverParticipant, wait time.Duration, title, message string) {
	f := a.failovers
	f.mu.Lock()
	pid, name := st.cfg.GetProjectId(), st.cfg.GetContainer()
	f.mu.Unlock()
	go func() {
		defer func() {
			f.mu.Lock()
			st.restoring = false
			f.mu.Unlock()
			a.changed()
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		err := a.restoreFromReplica(ctx, pid, name, me.GetDir(), func(string) {})
		if err != nil && (!a.projects.Has(ctx, pid, name) || errors.Is(err, errAlreadyHere)) {
			a.log.Error("用副本启动容器失败", "project", pid, "container", name, "err", err)
			a.alerts.Fire(ctx, "takeover:"+id, "容器 "+name+" 用副本启动失败", err.Error(), pid)
			return
		}
		if err != nil {
			a.log.Warn("容器已恢复，但启动失败", "project", pid, "container", name, "err", err)
		}
		a.log.Info(title, "project", pid, "container", name)
		a.alerts.Notify(ctx, title, message, pid)
		f.mu.Lock()
		st.nextSync = time.Now() // 立即开始同步给对方（它现在是热备）
		f.mu.Unlock()
	}()
}

var errAlreadyHere = errors.New("本节点上已经有这个容器，没有用副本恢复")

// restoreFromReplica 用副本把容器恢复到本节点的 dir 并启动，成功放到本节点上以后删除副本。
func (a *agent) restoreFromReplica(ctx context.Context, pid, name, dir string, onLine func(string)) error {
	_, sp, root, err := a.replicas.load(pid, name)
	if err != nil {
		return err
	}
	sp = proto.Clone(sp).(*nagiv1.ContainerSpec)
	if dir != "" {
		sp.Dir = dir
	}
	unlock := a.replicas.lock(pid, name)
	defer unlock()
	if a.projects.Has(ctx, pid, name) {
		// 不应该出现：本节点上已经有这个容器（缓存），副本保留，等人工处理。
		return errAlreadyHere
	}
	err = a.projects.RestoreReplica(ctx, sp, filepath.Join(root, "files", "repo"), "", onLine)
	// 容器已经放到本节点上（启动失败也算），用自己的快照仓库了，副本没用了。
	if a.projects.Has(ctx, pid, name) {
		os.RemoveAll(root)
		os.RemoveAll(a.replicaStateDir(pid, name))
	}
	a.changed()
	return err
}

// failback 把容器交还给原节点（A）：同步一次、停止容器、再同步最后的数据，然后把租约转让给 A，本节点转为热备。
// 转让之前的任何一步失败，都在本节点重新启动容器，过 failbackAfter 再试。调用方已经把 st.failingBack 设为 true。
func (a *agent) failback(ctx context.Context, id string) {
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return
	}
	f.mu.Lock()
	cfg, epoch := st.cfg, st.epoch
	f.mu.Unlock()
	me, other := a.selfParticipant(cfg)
	if me == nil || other == nil {
		return
	}
	pid, name := cfg.GetProjectId(), cfg.GetContainer()
	target := &nagiv1.BackupTarget{NodeId: other.GetNodeId(), Name: other.GetName(), Key: other.GetKey()}
	done := func(err error) {
		f.mu.Lock()
		st.failingBack = false
		if err != nil {
			st.preferredSince = time.Now() // 过一阵再试
			st.lastError = "切回原节点失败：" + err.Error()
		}
		f.mu.Unlock()
		a.changed()
		if err != nil {
			a.log.Warn("切回原节点失败，容器留在本节点", "project", pid, "container", name, "err", err)
			a.alerts.Fire(ctx, "failback:"+id, "容器 "+name+" 切回原节点失败", err.Error()+"。容器继续在本节点运行，过一阵会再试", pid)
		}
	}
	a.log.Info("原节点已稳定，切回容器", "project", pid, "container", name, "to", other.GetName())
	a.changed()
	// 先在容器运行时同步一次，停止以后只需要同步很少的增量，停机时间短。
	if _, err := a.syncTo(ctx, pid, name, target); err != nil {
		done(err)
		return
	}
	if err := a.projects.MigrateStop(ctx, pid, name, func(string) {}); err != nil {
		_ = a.projects.MigrateResume(ctx, pid, name, func(string) {})
		done(err)
		return
	}
	resume := func(err error) {
		if rerr := a.projects.MigrateResume(ctx, pid, name, func(string) {}); rerr != nil {
			a.log.Error("切回失败后重新启动容器也失败了", "project", pid, "container", name, "err", rerr)
		}
		done(err)
	}
	if _, err := a.syncTo(ctx, pid, name, target); err != nil {
		resume(err)
		return
	}
	// 转让租约：先请对方和见证人转票，拿到其中一票（加上自己的就是多数）才算数。
	newEpoch := epoch + 1
	if n := a.collectHandover(ctx, []*nagiv1.FailoverParticipant{other, cfg.GetWitness()}, cfg, other.GetKey(), newEpoch); n == 0 {
		resume(errors.New("对方和见证人都没有同意转让租约"))
		return
	}
	if ok, _, _ := f.voter.Handover(id, participants(cfg), me.GetKey(), other.GetKey(), newEpoch, leaseOf(cfg)); !ok {
		// 已经有一方转了票，不能再在本节点启动（可能两边同时运行）；对方凑够两票就会接手。
		a.log.Warn("本节点的票没能转出，等对方接手", "project", pid, "container", name)
	}
	done(nil)
	a.demote(ctx, id, other.GetKey(), newEpoch, false)
	a.alerts.Resolve(ctx, "failback:"+id)
	a.alerts.Notify(ctx, "容器已切回原节点", "原节点 "+other.GetName()+" 已经稳定，容器 "+name+" 已交还给它", pid)
}

// collectHandover 请 others 把票转让给 to（第 epoch 轮），返回同意的数量。
func (a *agent) collectHandover(ctx context.Context, others []*nagiv1.FailoverParticipant, cfg *nagiv1.FailoverConfig, to []byte, epoch uint64) int {
	var n int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range others {
		wg.Add(1)
		go func(m *nagiv1.FailoverParticipant) {
			defer wg.Done()
			resp, err := a.requestVote(ctx, m, cfg, epoch, to)
			if err == nil && resp.GetGranted() {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	return n
}

// demote 本节点已经被顶替（或主动切回给了原节点）：停止并删除容器，目录改名保留，转为新持有者的热备。
// notify 为 false 时不发"已被顶替"的通知（自动切回是本节点主动交出去的，另外发通知）。
func (a *agent) demote(ctx context.Context, id string, holder []byte, epoch uint64, notify bool) {
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return
	}
	f.mu.Lock()
	cfg := st.cfg
	f.mu.Unlock()
	pid, name := cfg.GetProjectId(), cfg.GetContainer()
	holderName := participantName(cfg, holder)
	a.log.Warn("容器已被顶替，本节点转为热备", "project", pid, "container", name, "holder", holderName, "epoch", epoch)
	// 热备恢复时用的配置：本节点缓存的这份（目录是本节点上的）。
	var sp *nagiv1.ContainerSpec
	if c, err := a.projects.Get(ctx, pid, name); err == nil {
		sp = c.Spec
		kept, err := a.projects.Retire(ctx, pid, name, func(string) {})
		if err != nil {
			a.log.Error("移出被顶替的容器失败", "project", pid, "container", name, "err", err)
		} else {
			a.log.Info("原容器目录已改名保留", "dir", kept)
		}
	} else if _, old, _, err := a.replicas.load(pid, name); err == nil {
		sp = old
	}
	_ = f.voter.Set(id, holder, epoch, leaseOf(cfg))
	if sp != nil {
		_ = a.replicas.save(&replicaMeta{ProjectID: pid, Container: name, SourceKey: holder, SourceName: holderName}, sp)
	}
	f.mu.Lock()
	st.holder, st.epoch = holder, epoch
	st.fenced, st.renewed = false, false
	_ = f.save(id, st)
	f.mu.Unlock()
	a.changed()
	a.alerts.Resolve(ctx, "lease:"+id)
	if notify {
		a.alerts.Notify(ctx, "容器已被其他节点顶替", "容器 "+name+" 现在由 "+holderName+" 运行，本节点转为热备；原目录改名保留", pid)
	}
}

// prePull 热备收到同步后提前拉镜像（最多每 30 分钟一次）。
func (a *agent) prePull(ctx context.Context, pid, name string) {
	id := pid + "-" + name
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return
	}
	f.mu.Lock()
	st.lastSync = time.Now()
	_ = f.save(id, st)
	if time.Since(st.lastPull) < 30*time.Minute {
		f.mu.Unlock()
		return
	}
	st.lastPull = time.Now()
	f.mu.Unlock()
	_, sp, _, err := a.replicas.load(pid, name)
	if err != nil {
		return
	}
	if err := project.PrePull(ctx, sp, filepath.Join(a.cfg.DataDir, "replicas", ".prepull-"+id), func(string) {}); err != nil {
		a.log.Warn("热备提前拉取镜像失败（顶替时会再拉）", "project", pid, "container", name, "err", err)
	}
}

// managed 判断容器是否设置了顶替（由节点代管重启、每 15 分钟快照）。
func (a *agent) managed(pid, name string) bool {
	return a.failovers.get(pid+"-"+name) != nil
}

// ---- AgentService ----

func (s *agentService) SetFailover(ctx context.Context, req *nagiv1.SetFailoverRequest) (*nagiv1.FailoverStatus, error) {
	a := s.node
	f := a.failovers
	if req.GetClear() {
		id, err := replicaID(req.GetProjectId(), req.GetContainer())
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		if st := f.m[id]; st != nil && st.cancelLoop != nil {
			st.cancelLoop()
		}
		delete(f.m, id)
		os.Remove(f.file(id))
		f.mu.Unlock()
		f.voter.Forget(id)
		if a.projects.Has(ctx, req.GetProjectId(), req.GetContainer()) {
			if err := a.projects.SetManagedRestart(ctx, req.GetProjectId(), req.GetContainer(), false); err != nil {
				a.log.Warn("恢复容器的重启策略失败", "project", req.GetProjectId(), "container", req.GetContainer(), "err", err)
			}
		}
		for _, k := range []string{"lease:", "takeover:", "standby:", "failback:"} {
			a.alerts.Resolve(ctx, k+id)
		}
		a.changed()
		return &nagiv1.FailoverStatus{}, nil
	}
	cfg := req.GetConfig()
	id, err := replicaID(cfg.GetProjectId(), cfg.GetContainer())
	if err != nil {
		return nil, err
	}
	if len(cfg.GetA().GetKey()) != 32 || len(cfg.GetB().GetKey()) != 32 || len(cfg.GetWitness().GetKey()) != 32 {
		return nil, status.Error(codes.InvalidArgument, "顶替设置不完整")
	}
	if a.mesh == nil {
		return nil, status.Error(codes.FailedPrecondition, "mesh 没有启动")
	}
	pid, name := cfg.GetProjectId(), cfg.GetContainer()
	me, _ := a.selfParticipant(cfg)
	// 新设置时 A 上必须有容器；已有设置（例如顶替之后更换见证人）时 A 可能已经退位成了热备。
	if me != nil && bytes.Equal(me.GetKey(), cfg.GetA().GetKey()) && f.get(id) == nil && !a.projects.Has(ctx, pid, name) {
		return nil, status.Error(codes.FailedPrecondition, "A 节点上没有这个容器")
	}
	// 初始租约归 A，第 1 轮；更换见证人时新见证人从当前的持有者和轮次开始。
	// 已经有设置时（修改参数、更换见证人）保留当前的持有者和轮次。
	f.mu.Lock()
	st := f.m[id]
	if st == nil {
		st = &failoverState{holder: cfg.GetA().GetKey(), epoch: 1}
		if h := req.GetSeedHolder(); req.GetSeedEpoch() > 0 && (bytes.Equal(h, cfg.GetA().GetKey()) || bytes.Equal(h, cfg.GetB().GetKey())) {
			st.holder, st.epoch = h, req.GetSeedEpoch()
		}
		f.m[id] = st
	}
	st.cfg = cfg
	err = f.save(id, st)
	holder, epoch := st.holder, st.epoch
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, cur, _ := f.voter.State(id, leaseOf(cfg)); cur < epoch {
		if err := f.voter.Set(id, holder, epoch, leaseOf(cfg)); err != nil {
			return nil, err
		}
	}
	// 热备：副本由面板事先登记（AcceptReplica），这里确认接收的是持有者的同步。
	if me != nil && !bytes.Equal(me.GetKey(), holder) {
		m, sp, _, err := a.replicas.load(pid, name)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "热备上还没有登记这个容器的副本")
		}
		m.SourceKey, m.SourceName = holder, participantName(cfg, holder)
		if err := a.replicas.save(m, sp); err != nil {
			return nil, err
		}
	}
	if a.projects.Has(ctx, pid, name) {
		if err := a.projects.SetManagedRestart(ctx, pid, name, true); err != nil {
			a.log.Warn("修改容器的重启策略失败", "project", pid, "container", name, "err", err)
		}
	}
	a.startFailover(context.WithoutCancel(a.ctx), id)
	a.changed()
	return a.failoverStatus(ctx, id), nil
}

func (a *agent) failoverStatus(ctx context.Context, id string) *nagiv1.FailoverStatus {
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return &nagiv1.FailoverStatus{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := st.cfg
	out := &nagiv1.FailoverStatus{
		Config: cfg, HolderName: participantName(cfg, st.holder), HolderKey: st.holder, Epoch: st.epoch,
		PendingConfirm: st.pending, LastError: st.lastError,
	}
	me, _ := a.selfParticipant(cfg)
	switch {
	case me == nil:
		out.Role = "witness"
		// 见证人只投票：持有者和轮次以自己的票为准。
		if h, e, _ := f.voter.State(id, leaseOf(cfg)); len(h) > 0 {
			out.HolderName, out.HolderKey, out.Epoch = participantName(cfg, h), h, e
		}
	case bytes.Equal(me.GetKey(), st.holder):
		out.Role = "holder"
		if !st.fenced {
			out.LeaseRemainingMs = max(0, time.Until(st.leaseEnd).Milliseconds())
		}
		out.FailingBack = st.failingBack
		if !st.failingBack && !st.preferredSince.IsZero() {
			out.FailbackInMs = max(1, time.Until(st.preferredSince.Add(failbackAfter)).Milliseconds())
		}
	default:
		out.Role = "standby"
	}
	if !st.lastSync.IsZero() {
		out.LastSync = timestamppb.New(st.lastSync)
	}
	if c, err := a.projects.Get(ctx, cfg.GetProjectId(), cfg.GetContainer()); err == nil {
		out.Stateful = c.Spec.GetStateful()
	} else if _, sp, _, err := a.replicas.load(cfg.GetProjectId(), cfg.GetContainer()); err == nil {
		out.Stateful = sp.GetStateful()
		out.SpecRevision, out.SpecProjectRevision = sp.GetRevision(), sp.GetProjectRevision()
	}
	return out
}

// failoverStatuses 返回本节点参与的所有顶替的状态（上报给面板）。
func (a *agent) failoverStatuses(ctx context.Context) []*nagiv1.FailoverStatus {
	var out []*nagiv1.FailoverStatus
	for _, id := range a.failovers.ids() {
		out = append(out, a.failoverStatus(ctx, id))
	}
	return out
}

func (s *agentService) ConfirmTakeover(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.FailoverStatus, error) {
	id, err := replicaID(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	f := s.node.failovers
	st := f.get(id)
	if st == nil {
		return nil, status.Error(codes.NotFound, "本节点没有这个容器的顶替设置")
	}
	f.mu.Lock()
	if !st.pending {
		f.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "现在不需要确认顶替")
	}
	st.confirmed = true
	f.mu.Unlock()
	return s.node.failoverStatus(ctx, id), nil
}
