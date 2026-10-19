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
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 顶替（见 docs/failover.md）。每个设置了顶替的项目有 A、B 两个能运行它的节点和一个见证人，
// 三方各一票，拿到 2 票（租约）的一方运行项目：
//   - 持有者每 1/3 个租约时长续约一次。续不上时，在"租约到期前 5 秒 + 停止超时"开始停止项目，
//     到期前 5 秒还没停完就强制结束；之后又续上了就重新启动。
//   - 热备平时接收持有者的同步、提前拉好镜像。它投给持有者的票作废（超过租约时长没收到续约）后，
//     先向见证人和原持有者要票，拿到一票再投自己，凑够 2 票就成为新的持有者（轮次加一），
//     再等 5 秒，用副本恢复并启动项目。有状态项目默认先告警，等用户确认。
//   - 原持有者回来后续约被拒绝，看到更高的轮次，就停掉项目、把目录改名保留，转为新持有者的热备。
//   - 自动切回（auto_failback）：项目不在原节点 A 上运行、而 A 已经连续稳定 failbackAfter 时，持有者有计划地
//     把项目交还给 A：同步一次、停止项目、再同步最后的数据，然后把租约转让给 A（自己的票、见证人的票、A 的票，
//     轮次加一）。A 发现自己成了持有者、手上只有副本，就用副本恢复并启动。和手动迁移一样不丢数据。

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
	m     map[string]*failoverState
}

func newFailovers(dataDir string) *failovers {
	dir := filepath.Join(dataDir, "failover")
	return &failovers{dir: dir, voter: lease.NewVoter(filepath.Join(dir, "votes")), m: map[string]*failoverState{}}
}

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
	tmp := f.file(id) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.file(id))
}

func (f *failovers) loadAll() {
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !projectIDRe.MatchString(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			continue
		}
		var ff failoverFile
		cfg := &nagiv1.FailoverConfig{}
		if json.Unmarshal(b, &ff) != nil || protojson.Unmarshal(ff.Config, cfg) != nil {
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

// ---- 角色 ----

func (a *agent) selfKey() []byte {
	if a.mesh == nil {
		return nil
	}
	k := a.mesh.m.PublicKey()
	return k[:]
}

// self 返回本节点在设置里是 A 还是 B（是见证人时返回 nil），以及另一个节点。
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

func (s *meshService) LeaseVote(ctx context.Context, req *nagiv1.LeaseVoteRequest) (*nagiv1.LeaseVoteResponse, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	f := s.a.failovers
	st := f.get(req.GetProjectId())
	if st == nil {
		return nil, status.Error(codes.NotFound, "本节点没有这个项目的顶替设置")
	}
	if to := req.GetHandoverTo(); len(to) > 0 {
		granted, holder, epoch := f.voter.Handover(req.GetProjectId(), participants(st.cfg), caller[:], to, req.GetEpoch(), leaseOf(st.cfg))
		return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}, nil
	}
	granted, holder, epoch := f.voter.Vote(req.GetProjectId(), participants(st.cfg), caller[:], req.GetEpoch(), leaseOf(st.cfg))
	return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}, nil
}

// requestVote 向另一个参与者要票；handoverTo 不为空时是请它把票转让给这一方。
func (a *agent) requestVote(ctx context.Context, m *nagiv1.FailoverParticipant, id string, epoch uint64, handoverTo ...[]byte) (*nagiv1.LeaseVoteResponse, error) {
	k, err := mesh.ParseKey(m.GetKey())
	if err != nil {
		return nil, err
	}
	cc, err := mesh.Client(a.mesh.m, k)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req := &nagiv1.LeaseVoteRequest{ProjectId: id, Epoch: epoch}
	if len(handoverTo) > 0 {
		req.HandoverTo = handoverTo[0]
	}
	return nagiv1.NewMeshServiceClient(cc).LeaseVote(ctx, req)
}

type voteResult struct {
	granted   int
	grantedBy [][]byte                  // 投了票的各方的公钥
	higher    *nagiv1.LeaseVoteResponse // 某一方告诉我们的更高轮次
}

// collect 并行向 others 要票。
func (a *agent) collect(ctx context.Context, others []*nagiv1.FailoverParticipant, id string, epoch uint64) voteResult {
	var r voteResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range others {
		wg.Add(1)
		go func(m *nagiv1.FailoverParticipant) {
			defer wg.Done()
			resp, err := a.requestVote(ctx, m, id, epoch)
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

// startFailover 启动一个项目的顶替循环（本节点是 A 或 B 时）。
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
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			a.failoverTick(lctx, id)
			select {
			case <-lctx.Done():
				return
			case <-t.C:
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
	L := leaseOf(cfg)
	now := time.Now()
	f.mu.Lock()
	due := !st.renewed || !now.Before(st.nextRenew)
	f.mu.Unlock()
	if due {
		sent := time.Now()
		r := a.collect(ctx, []*nagiv1.FailoverParticipant{other, cfg.GetWitness()}, id, epoch)
		self, _, _ := f.voter.Vote(id, participants(cfg), me.GetKey(), epoch, L)
		votes := r.granted
		if self {
			votes++
		}
		f.mu.Lock()
		st.renewed = true
		st.nextRenew = now.Add(L / 3)
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
		if startFailback && a.projects.Has(ctx, id) {
			go a.failback(context.WithoutCancel(ctx), id)
		} else if startFailback {
			f.mu.Lock()
			st.failingBack = false
			f.mu.Unlock()
		}
		if !ok && r.higher != nil {
			a.demote(ctx, id, r.higher.GetHolder(), r.higher.GetEpoch())
			return
		}
		if ok && wasFenced && a.projects.Has(ctx, id) {
			a.log.Info("重新拿到租约，启动项目", "project", id)
			if err := a.projects.Unfence(ctx, id, func(string) {}); err == nil {
				f.mu.Lock()
				st.fenced = false
				f.mu.Unlock()
				a.alerts.Resolve(ctx, "lease:"+id)
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
	// 刚启动时还要等 WireGuard 握手，头 10 秒内续不上先不停（Docker 不会自己拉起顶替项目的容器，
	// 这段时间项目本来就没在运行）。
	grace := !everOK && time.Since(a.startedAt) < startupGrace
	if !fenced && !restoring && !failingBack && !grace && time.Now().After(leaseEnd.Add(-safetyMargin-stopTimeout)) && a.projects.Has(ctx, id) {
		a.log.Warn("续不上租约，停止项目", "project", id)
		a.alerts.Fire(ctx, "lease:"+id, "续不上顶替租约，已停止项目", "联系不上另一个节点和见证人；恢复后会自动重新启动", id)
		_ = a.projects.Fence(ctx, id, leaseEnd.Add(-safetyMargin), func(string) {})
		f.mu.Lock()
		st.fenced = true
		f.mu.Unlock()
		// 下面的代管重启不能用停止前读到的状态，否则会把刚停掉的容器又拉起来。
		fenced = true
	}

	// 持有租约时，按用户原来的重启策略代为重启退出的容器；机器重启后也由这里把项目启动起来。
	if superviseDue && !fenced && !restoring && !failingBack && everOK && time.Now().Before(leaseEnd) && a.projects.Has(ctx, id) {
		a.projects.Supervise(ctx, id)
	}

	// 成了持有者、手上却只有副本：对方把项目交还给了本节点（自动切回），用副本恢复并启动。
	if everOK && !fenced && !restoring && !failingBack && !a.projects.Has(ctx, id) {
		if _, _, err := a.replicas.load(id); err == nil {
			f.mu.Lock()
			st.restoring = true
			f.mu.Unlock()
			a.startFromReplica(ctx, id, st, me, 0, "项目已切回本节点", "原节点恢复后，项目从 "+other.GetName()+" 切回了本节点")
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
	if syncDue && a.projects.Has(ctx, id) {
		go a.syncStandby(context.WithoutCancel(ctx), id)
	}
}

// syncStandby 持有者把副本同步给热备。
func (a *agent) syncStandby(ctx context.Context, id string) {
	st := a.failovers.get(id)
	if st == nil {
		return
	}
	a.failovers.mu.Lock()
	cfg := st.cfg
	a.failovers.mu.Unlock()
	_, other := a.selfParticipant(cfg)
	if other == nil {
		return
	}
	_, err := a.syncTo(ctx, id, &project.BackupTarget{NodeID: other.GetNodeId(), Name: other.GetName(), Key: other.GetKey()})
	a.failovers.mu.Lock()
	if err != nil && !bytes.Equal(st.holder, a.selfKey()) {
		// 同步的过程中本节点被顶替、退为热备了：对方拒绝是正常的，不算失败。
		a.failovers.mu.Unlock()
		return
	}
	if err != nil {
		st.lastError = "同步给热备失败：" + err.Error()
		st.nextSync = time.Now().Add(time.Minute) // 热备可能暂时不在，1 分钟后重试
	} else {
		st.lastError, st.lastSync = "", time.Now()
		_ = a.failovers.save(id, st)
	}
	a.failovers.mu.Unlock()
	if err != nil {
		a.alerts.Fire(ctx, "standby:"+id, "同步给热备节点失败", err.Error(), id)
	} else {
		a.alerts.Resolve(ctx, "standby:"+id)
	}
}

func (a *agent) standbyTick(ctx context.Context, id string, st *failoverState, cfg *nagiv1.FailoverConfig, me, other *nagiv1.FailoverParticipant) {
	f := a.failovers
	L := leaseOf(cfg)
	holder, epoch, expired := f.voter.State(id, L)
	f.mu.Lock()
	if epoch > st.epoch {
		st.holder, st.epoch = holder, epoch
		_ = f.save(id, st)
	}
	if !expired {
		st.pending, st.confirmed = false, false
		f.mu.Unlock()
		a.alerts.Resolve(ctx, "takeover:"+id)
		return
	}
	confirmed, pending := st.confirmed, st.pending
	f.mu.Unlock()

	// 持有者失联。
	_, root, err := a.replicas.load(id)
	if err != nil {
		f.mu.Lock()
		st.lastError = "持有者失联，但热备还没有收到过同步，无法顶替"
		f.mu.Unlock()
		a.alerts.Fire(ctx, "takeover:"+id, "项目无法顶替", "热备还没有收到过同步", "")
		return
	}
	files := filepath.Join(root, "files")
	if project.ReplicaStateful(files) && !cfg.GetAutoStateful() && !confirmed {
		if !pending {
			f.mu.Lock()
			st.pending = true
			f.mu.Unlock()
			a.log.Warn("持有者失联，有状态项目等待确认顶替", "project", id)
			a.alerts.Fire(ctx, "takeover:"+id, "项目所在节点失联，等待确认顶替",
				"有状态项目：顶替会丢失最后一次同步之后写入的数据。请在面板上确认", "")
		}
		return
	}

	// 先向见证人和原持有者要票，拿到一票再投自己。
	newEpoch := epoch + 1
	sent := time.Now()
	r := a.collect(ctx, []*nagiv1.FailoverParticipant{cfg.GetWitness(), other}, id, newEpoch)
	if r.granted == 0 {
		return
	}
	if ok, _, _ := f.voter.Vote(id, participants(cfg), me.GetKey(), newEpoch, L); !ok {
		return
	}
	a.log.Warn("拿到多数票，顶替项目", "project", id, "epoch", newEpoch)
	f.mu.Lock()
	st.holder, st.epoch = me.GetKey(), newEpoch
	st.leaseEnd = sent.Add(L)
	st.nextRenew = time.Now().Add(L / 3)
	st.renewed = true
	st.restoring = true
	st.pending, st.confirmed = false, false
	_ = f.save(id, st)
	f.mu.Unlock()
	a.alerts.Resolve(ctx, "takeover:"+id)

	a.startFromReplica(ctx, id, st, me, safetyMargin, "已顶替项目", "原节点 "+participantName(cfg, holder)+" 失联，项目已在本节点启动")
}

// startFromReplica 成为持有者以后在后台用副本恢复并启动项目（顶替和切回共用），循环照常续约；租约丢了就取消恢复。
// wait 是开始前先等的时间（顶替时等原持有者的租约彻底过期）。调用方已经把 st.restoring 设为 true。
func (a *agent) startFromReplica(ctx context.Context, id string, st *failoverState, me *nagiv1.FailoverParticipant, wait time.Duration, title, message string) {
	f := a.failovers
	go func() {
		defer func() {
			f.mu.Lock()
			st.restoring = false
			f.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		var lines []string
		_, err := a.restoreReplica(ctx, id, me.GetDir(), "", func(l string) { lines = append(lines, l) })
		if err != nil {
			a.log.Error("用副本启动项目失败", "project", id, "err", err)
			a.alerts.Fire(ctx, "takeover:"+id, "用副本启动项目失败", err.Error(), "")
			return
		}
		a.log.Info(title, "project", id)
		a.alerts.Notify(ctx, title, message, id)
		a.changed()
		f.mu.Lock()
		st.nextSync = time.Now() // 立即开始同步给对方（它现在是热备）
		f.mu.Unlock()
	}()
}

// failback 把项目交还给原节点（A）：同步一次、停止项目、再同步最后的数据，然后把租约转让给 A，本节点转为热备。
// 转让之前的任何一步失败，都在本节点重新启动项目，过 failbackAfter 再试。调用方已经把 st.failingBack 设为 true。
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
	target := &project.BackupTarget{NodeID: other.GetNodeId(), Name: other.GetName(), Key: other.GetKey()}
	done := func(err error) {
		f.mu.Lock()
		st.failingBack = false
		if err != nil {
			st.preferredSince = time.Now() // 过一阵再试
			st.lastError = "切回原节点失败：" + err.Error()
		}
		f.mu.Unlock()
		if err != nil {
			a.log.Warn("切回原节点失败，项目留在本节点", "project", id, "err", err)
			a.alerts.Fire(ctx, "failback:"+id, "切回原节点失败", err.Error()+"。项目继续在本节点运行，过一阵会再试", id)
		}
	}
	a.log.Info("原节点已稳定，切回项目", "project", id, "to", other.GetName())
	// 先在项目运行时同步一次，停止以后只需要同步很少的增量，停机时间短。
	if _, err := a.syncTo(ctx, id, target); err != nil {
		done(err)
		return
	}
	if _, err := a.projects.MigrateStop(ctx, id, func(string) {}); err != nil {
		_ = a.projects.MigrateResume(ctx, id, func(string) {})
		done(err)
		return
	}
	resume := func(err error) {
		if rerr := a.projects.MigrateResume(ctx, id, func(string) {}); rerr != nil {
			a.log.Error("切回失败后重新启动项目也失败了", "project", id, "err", rerr)
		}
		done(err)
	}
	if _, err := a.syncTo(ctx, id, target); err != nil {
		resume(err)
		return
	}
	// 转让租约：先请对方和见证人转票，拿到其中一票（加上自己的就是多数）才算数。
	newEpoch := epoch + 1
	if n := a.collectHandover(ctx, []*nagiv1.FailoverParticipant{other, cfg.GetWitness()}, id, other.GetKey(), newEpoch); n == 0 {
		resume(errors.New("对方和见证人都没有同意转让租约"))
		return
	}
	if ok, _, _ := f.voter.Handover(id, participants(cfg), me.GetKey(), other.GetKey(), newEpoch, leaseOf(cfg)); !ok {
		// 已经有一方转了票，不能再在本节点启动（可能两边同时运行）；对方凑够两票就会接手。
		a.log.Warn("本节点的票没能转出，等对方接手", "project", id)
	}
	done(nil)
	a.demote(ctx, id, other.GetKey(), newEpoch, false)
	a.alerts.Resolve(ctx, "failback:"+id)
	a.alerts.Notify(ctx, "项目已切回原节点", "原节点 "+other.GetName()+" 已经稳定，项目已交还给它", id)
}

// collectHandover 请 others 把票转让给 to（第 epoch 轮），返回同意的数量。
func (a *agent) collectHandover(ctx context.Context, others []*nagiv1.FailoverParticipant, id string, to []byte, epoch uint64) int {
	var n int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range others {
		wg.Add(1)
		go func(m *nagiv1.FailoverParticipant) {
			defer wg.Done()
			resp, err := a.requestVote(ctx, m, id, epoch, to)
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

// demote 本节点已经被顶替：停止项目，目录改名保留，转为新持有者的热备。
// notify 为 false 时不发"已被顶替"的通知（自动切回是本节点主动交出去的，另外发通知）。
func (a *agent) demote(ctx context.Context, id string, holder []byte, epoch uint64, notify ...bool) {
	f := a.failovers
	st := f.get(id)
	if st == nil {
		return
	}
	f.mu.Lock()
	cfg := st.cfg
	f.mu.Unlock()
	name := participantName(cfg, holder)
	a.log.Warn("项目已被顶替，本节点转为热备", "project", id, "holder", name, "epoch", epoch)
	if a.projects.Has(ctx, id) {
		kept, err := a.projects.Demote(ctx, id, func(string) {})
		if err != nil {
			a.log.Error("移出被顶替的项目失败", "project", id, "err", err)
		} else {
			a.log.Info("原项目目录已改名保留", "dir", kept)
		}
		a.changed()
	}
	_ = f.voter.Set(id, holder, epoch, leaseOf(cfg))
	_ = a.replicas.save(&replicaMeta{ProjectID: id, ProjectName: cfg.GetProjectName(), SourceKey: holder, SourceName: name})
	f.mu.Lock()
	st.holder, st.epoch = holder, epoch
	st.fenced, st.renewed = false, false
	_ = f.save(id, st)
	f.mu.Unlock()
	a.alerts.Resolve(ctx, "lease:"+id)
	if len(notify) == 0 || notify[0] {
		a.alerts.Notify(ctx, "项目已被其他节点顶替", "现在由 "+name+" 运行，本节点转为热备；原目录改名保留", "")
	}
}

// restoreReplica 把副本恢复成项目并删除副本（手动恢复和顶替共用）。
func (a *agent) restoreReplica(ctx context.Context, id, dir, snapshotID string, onLine func(string)) (*nagiv1.Project, error) {
	_, root, err := a.replicas.load(id)
	if err != nil {
		return nil, err
	}
	unlock := a.replicas.lock(id)
	defer unlock()
	proj, err := a.projects.RestoreReplica(ctx, id, filepath.Join(root, "files"), dir, snapshotID, onLine)
	if proj != nil {
		// 项目已经有了自己的快照仓库，副本不再需要。
		os.RemoveAll(root)
		os.RemoveAll(a.replicaStateDir(id))
		onLine("已删除副本，项目使用自己的快照仓库")
	}
	return proj, err
}

// prePull 热备收到同步后提前拉镜像（最多每 30 分钟一次）。
func (a *agent) prePull(ctx context.Context, id string) {
	st := a.failovers.get(id)
	if st == nil {
		return
	}
	a.failovers.mu.Lock()
	if time.Since(st.lastPull) < 30*time.Minute {
		a.failovers.mu.Unlock()
		return
	}
	st.lastPull = time.Now()
	st.lastSync = time.Now()
	_ = a.failovers.save(id, st)
	a.failovers.mu.Unlock()
	_, root, err := a.replicas.load(id)
	if err != nil {
		return
	}
	if err := project.PrePull(ctx, filepath.Join(root, "files"), func(string) {}); err != nil {
		a.log.Warn("热备提前拉取镜像失败（顶替时会再拉）", "project", id, "err", err)
	}
}

// ---- AgentService ----

func (s *agentService) SetFailover(ctx context.Context, req *nagiv1.SetFailoverRequest) (*nagiv1.FailoverStatus, error) {
	a := s.node
	f := a.failovers
	if req.GetClear() {
		id := req.GetProjectId()
		f.mu.Lock()
		if st := f.m[id]; st != nil && st.cancelLoop != nil {
			st.cancelLoop()
		}
		delete(f.m, id)
		os.Remove(f.file(id))
		f.mu.Unlock()
		f.voter.Forget(id)
		if a.projects.Has(ctx, id) {
			if err := a.projects.SetManagedRestart(ctx, id, false); err != nil {
				a.log.Warn("恢复容器的重启策略失败", "project", id, "err", err)
			}
		}
		for _, k := range []string{"lease:", "takeover:", "standby:"} {
			a.alerts.Resolve(ctx, k+id)
		}
		return &nagiv1.FailoverStatus{}, nil
	}
	cfg := req.GetConfig()
	id := cfg.GetProjectId()
	if !projectIDRe.MatchString(id) || len(cfg.GetA().GetKey()) != 32 || len(cfg.GetB().GetKey()) != 32 || len(cfg.GetWitness().GetKey()) != 32 {
		return nil, status.Error(codes.InvalidArgument, "顶替设置不完整")
	}
	if a.mesh == nil {
		return nil, status.Error(codes.FailedPrecondition, "mesh 没有启动")
	}
	me, _ := a.selfParticipant(cfg)
	// 新设置时 A 上必须有项目；已有设置（例如顶替之后更换见证人）时 A 可能已经退位成了热备。
	if me != nil && bytes.Equal(me.GetKey(), cfg.GetA().GetKey()) && f.get(id) == nil && !a.projects.Has(ctx, id) {
		return nil, status.Error(codes.FailedPrecondition, "A 节点上没有这个项目")
	}
	// 顶替和备份节点二选一：两套同步会写备份节点上同一个副本目录。
	if me != nil && bytes.Equal(me.GetKey(), cfg.GetA().GetKey()) && f.get(id) == nil {
		if bs, err := a.projects.BackupState(ctx, id); err == nil && bs.Target != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "这个项目设了备份节点 %s，先取消备份节点再设顶替（顶替节点本身就有一份随时更新的副本）", bs.Target.Name)
		}
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
	err := f.save(id, st)
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
	// 热备接收持有者的同步。
	if me != nil && !bytes.Equal(me.GetKey(), holder) {
		if err := a.replicas.save(&replicaMeta{ProjectID: id, ProjectName: cfg.GetProjectName(), SourceKey: holder,
			SourceNodeID: cfg.GetA().GetNodeId(), SourceName: participantName(cfg, holder)}); err != nil {
			return nil, err
		}
	}
	if a.projects.Has(ctx, id) {
		if err := a.projects.SetManagedRestart(ctx, id, true); err != nil {
			a.log.Warn("修改容器的重启策略失败", "project", id, "err", err)
		}
	}
	a.startFailover(context.WithoutCancel(a.ctx), id)
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
	out := &nagiv1.FailoverStatus{
		Config: st.cfg, HolderName: participantName(st.cfg, st.holder), HolderKey: st.holder, Epoch: st.epoch,
		PendingConfirm: st.pending, LastError: st.lastError,
	}
	me, _ := a.selfParticipant(st.cfg)
	switch {
	case me == nil:
		out.Role = "witness"
		// 见证人只投票：持有者和轮次以自己的票为准。
		if h, e, _ := f.voter.State(id, leaseOf(st.cfg)); len(h) > 0 {
			out.HolderName, out.HolderKey, out.Epoch = participantName(st.cfg, h), h, e
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
	if out.Role == "holder" {
		out.Stateful = a.projects.Stateful(ctx, id)
	} else if _, root, err := a.replicas.load(id); err == nil {
		out.Stateful = project.ReplicaStateful(filepath.Join(root, "files"))
	}
	return out
}

func (s *agentService) GetFailover(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.FailoverStatus, error) {
	return s.node.failoverStatus(ctx, req.GetProjectId()), nil
}

func (s *agentService) ListFailovers(ctx context.Context, _ *nagiv1.ListFailoversRequest) (*nagiv1.ListFailoversResponse, error) {
	f := s.node.failovers
	f.mu.Lock()
	ids := make([]string, 0, len(f.m))
	for id := range f.m {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	out := &nagiv1.ListFailoversResponse{}
	for _, id := range ids {
		out.Failovers = append(out.Failovers, s.node.failoverStatus(ctx, id))
	}
	return out, nil
}

func (s *agentService) ConfirmTakeover(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.FailoverStatus, error) {
	f := s.node.failovers
	st := f.get(req.GetProjectId())
	if st == nil {
		return nil, status.Error(codes.NotFound, "本节点没有这个项目的顶替设置")
	}
	f.mu.Lock()
	if !st.pending {
		f.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "现在不需要确认顶替")
	}
	st.confirmed = true
	f.mu.Unlock()
	return s.node.failoverStatus(ctx, req.GetProjectId()), nil
}
