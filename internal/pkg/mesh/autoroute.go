package mesh

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// 自动切路（见 docs/mesh.md#自动切路）：默认关闭，打开后观察有流量、直连可用的节点对，
// 按 ComparePaths 的建议把发给对端的 WireGuard 包改经中转池里的节点（对端公钥不变，隧道里的连接不断）。
// 已经建立的 QUIC 连接不迁移；用着中转时新的连接走 WireGuard，跟着走中转。
const (
	autoGap       = 4 * time.Minute  // 一个观察窗口结束到下一个开始至少隔这么久（整个节点只有一个窗口）
	autoBusy      = 1 << 20          // 两次挑选之间收发超过这么多字节的节点对才观察
	autoStreak    = 2                // 连续这么多个窗口给出同一个新建议才切换
	autoCooldown  = 10 * time.Minute // 切换以后这么久内不因比较再切（试用不通过、健康检查失败除外）
	autoHealth    = time.Second      // 用着中转时每秒经它做一次端到端探测
	autoFailLimit = 3                // 连续这么多次探测失败就回到直连
	autoBan       = 10 * time.Minute // 回退以后这条中转对这个对端这么久不用
	autoMaxRelays = 2                // 每个窗口最多比较几个中转（ObservePaths 的上限）
)

// autoRoute 是自动切路的控制器。状态只在 run 的 goroutine 里改。
type autoRoute struct {
	m       *Mesh
	observe func(ctx context.Context, peer Key, relays []Key) PathAdvice
	gap     time.Duration
	busy    uint64
	health  time.Duration
	state   map[Key]*routeState
	bytes   map[Key][2]uint64 // 上次挑选时的收发字节数
	heavy   map[Key]bool      // 发给它的流量超过中转限速的一半：切到中转会被限速，不切
	at      time.Time
	last    Key
}

// routeState 是一个对端的切路状态。
type routeState struct {
	streakVia Key // 连续给出的新建议（零值是直连）和次数
	streak    int
	trial     bool      // 刚切到中转，还要下一个窗口再确认一次
	quiet     time.Time // 冷却到这个时间
	fails     int       // 健康检查连续失败的次数
	banned    map[Key]time.Time
}

type healthResult struct {
	peer, via Key
	failed    bool
}

// SetAutoRoute 打开或关闭自动切路（默认关闭）。关闭时所有性能路线立即恢复直连。
func (m *Mesh) SetAutoRoute(on bool) { m.setAutoRoute(on, nil) }

// AutoRoute 返回自动切路是否打开。
func (m *Mesh) AutoRoute() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.autoStop != nil
}

func (m *Mesh) setAutoRoute(on bool, a *autoRoute) {
	m.mu.Lock()
	select {
	case <-m.done:
		on = false
	default:
	}
	if on == (m.autoStop != nil) {
		m.mu.Unlock()
		return
	}
	b := m.bind
	if !on {
		m.autoStop()
		m.autoStop = nil
		m.mu.Unlock()
		b.mu.Lock()
		b.autoRoute = false
		for _, p := range b.peers {
			p.perfVia = Key{}
		}
		b.mu.Unlock()
		m.log.Info("mesh 自动切路已关闭，全部恢复直连")
		return
	}
	if a == nil {
		a = &autoRoute{gap: autoGap, busy: autoBusy, health: autoHealth}
		a.observe = func(ctx context.Context, peer Key, relays []Key) PathAdvice {
			samples, err := m.ObservePaths(ctx, peer, relays)
			if err != nil {
				return PathAdvice{Reason: err.Error()}
			}
			return m.ComparePaths(peer, samples)
		}
	}
	a.m, a.state, a.bytes, a.heavy = m, map[Key]*routeState{}, map[Key][2]uint64{}, map[Key]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	m.autoStop = cancel
	m.mu.Unlock()
	b.mu.Lock()
	b.autoRoute = true
	b.mu.Unlock()
	m.log.Info("mesh 自动切路已打开")
	go a.run(ctx)
}

// perfHop 返回发给 p 的包现在要经过的性能中转，不用时为 nil。调用时持有 b.mu。
// 总开关关了、中转退出中转池或撤权、到中转的直连断了，都在下一个包回到直连，不等控制器。
func (b *Bind) perfHop(p *peerState, now time.Time) *peerState {
	if !b.autoRoute || p.perfVia == (Key{}) {
		return nil
	}
	rp := b.peers[p.perfVia]
	if !rp.poolRelay() || !rp.directAlive(now) || b.sock(rp.directConn) == nil {
		return nil
	}
	return rp
}

// perfRouted：发给 k 的业务现在经性能中转。新的连接不走 QUIC（QUIC 绑物理直连），改走 WireGuard 跟着走中转。
func (m *Mesh) perfRouted(k Key) bool {
	b := m.bind
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[k]
	return p != nil && p.directAlive(now) && b.perfHop(p, now) != nil
}

func (a *autoRoute) run(ctx context.Context) {
	health := time.NewTicker(a.health)
	defer health.Stop()
	type observed struct {
		peer Key
		adv  PathAdvice
	}
	results := make(chan observed, 1)
	checked := make(chan []healthResult, 1)
	observing, checking := false, false
	a.pick(time.Now()) // 只记下流量基线：打开以后先看一个周期的流量，再挑要观察的节点对
	next := time.Now().Add(a.gap)
	for {
		if !observing && !time.Now().Before(next) {
			if peer, relays, ok := a.pick(time.Now()); ok {
				observing = true
				go func() {
					results <- observed{peer, a.observe(ctx, peer, relays)}
				}()
			} else {
				next = time.Now().Add(a.gap)
			}
		}
		wait := time.NewTimer(max(time.Until(next), time.Millisecond))
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case r := <-results:
			observing = false
			next = time.Now().Add(a.gap)
			if ctx.Err() == nil {
				a.apply(r.peer, r.adv, time.Now())
			}
		case <-health.C:
			if !checking {
				if probes := a.routed(); len(probes) > 0 {
					checking = true
					go func() { checked <- a.probe(ctx, probes) }()
				}
			}
		case rs := <-checked:
			checking = false
			if ctx.Err() == nil {
				a.healthDone(rs, time.Now())
			}
		case <-wait.C:
		}
		wait.Stop()
	}
}

func (a *autoRoute) stateFor(k Key) *routeState {
	st := a.state[k]
	if st == nil {
		st = &routeState{banned: map[Key]time.Time{}}
		a.state[k] = st
	}
	return st
}

// pick 挑下一个要观察的对端和最多两个候选中转，轮流挑。对端要直连可用、不是面板，
// 并且上次挑选以来有业务流量（正在用中转的照样观察，才能回到直连）；中转要在中转池里、和这一端直连、没有被暂停使用。
// 正在用的中转排第一，其余按到它的直连延迟。
func (a *autoRoute) pick(now time.Time) (Key, []Key, bool) {
	busy := map[Key]bool{}
	elapsed := now.Sub(a.at).Seconds()
	for _, s := range a.m.Status() {
		cur := [2]uint64{s.RxBytes, s.TxBytes}
		prev, seen := a.bytes[s.Key]
		a.bytes[s.Key] = cur
		// 第一次看到（刚打开、新成员）或者计数归零（成员重新加入）时只记下基线。
		if !seen || cur[0] < prev[0] || cur[1] < prev[1] {
			continue
		}
		busy[s.Key] = cur[0]+cur[1]-prev[0]-prev[1] >= a.busy
		a.heavy[s.Key] = elapsed > 0 && float64(cur[1]-prev[1])/elapsed > relayRate/2
	}
	a.at = now
	b := a.m.bind
	b.mu.Lock()
	defer b.mu.Unlock()
	panels := map[Key]bool{}
	for _, p := range b.peers {
		for _, r := range p.relays {
			panels[r] = true
		}
	}
	type option struct {
		peer   Key
		relays []Key
	}
	var options []option
	for k, p := range b.peers {
		if panels[k] || !p.directAlive(now) || (!busy[k] && p.perfVia == (Key{})) {
			continue
		}
		st := a.stateFor(k)
		var pool []*peerState
		for rk, rp := range b.peers {
			if rk != k && rk != p.perfVia && rp.poolRelay() && rp.directAlive(now) && !now.Before(st.banned[rk]) {
				pool = append(pool, rp)
			}
		}
		slices.SortFunc(pool, func(x, y *peerState) int { return int(x.rtt - y.rtt) })
		var relays []Key
		if b.perfHop(p, now) != nil {
			relays = append(relays, p.perfVia)
		}
		for _, rp := range pool {
			if len(relays) < autoMaxRelays {
				relays = append(relays, rp.key)
			}
		}
		if len(relays) > 0 {
			options = append(options, option{k, relays})
		}
	}
	if len(options) == 0 {
		return Key{}, nil, false
	}
	slices.SortFunc(options, func(x, y option) int { return bytes.Compare(x.peer[:], y.peer[:]) })
	i := slices.IndexFunc(options, func(o option) bool { return bytes.Compare(o.peer[:], a.last[:]) > 0 })
	if i < 0 {
		i = 0
	}
	a.last = options[i].peer
	return options[i].peer, options[i].relays, true
}

// apply 按一个窗口的建议决定要不要切。证据不够时什么都不改；新建议要连续 autoStreak 个窗口一致、
// 不在冷却期才切；刚切到中转（试用期）的下一个窗口不再建议它，就马上回到直连并暂停用它。
func (a *autoRoute) apply(peer Key, adv PathAdvice, now time.Time) {
	st := a.stateFor(peer)
	b := a.m.bind
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[peer]
	if p == nil {
		delete(a.state, peer)
		return
	}
	cur := p.perfVia
	if !adv.Sufficient {
		st.streak = 0 // 证据断了要重新积累；现在的路线不动
		return
	}
	want := adv.Via
	if want != (Key{}) && (!b.peers[want].poolRelay() || now.Before(st.banned[want])) {
		want = Key{}
	}
	if want == cur {
		st.streak, st.trial = 0, false
		if cur != (Key{}) {
			p.perfRTT = adviceRTT(adv, cur)
		}
		return
	}
	if st.trial {
		p.perfVia = Key{}
		st.trial, st.streak, st.fails = false, 0, 0
		st.banned[cur], st.quiet = now.Add(autoBan), now.Add(autoCooldown)
		a.m.log.Info("mesh 自动切路试用没通过，回到直连", "peer", peer.Short(), "relay", cur.Short(), "reason", adv.Reason)
		return
	}
	if want == st.streakVia {
		st.streak++
	} else {
		st.streakVia, st.streak = want, 1
	}
	// 回直连只要连续两个窗口一致，不等冷却：直连是默认、最稳的路线；冷却只挡住再切到中转。
	if st.streak < autoStreak || (want != (Key{}) && now.Before(st.quiet)) {
		return
	}
	if want != (Key{}) && a.heavy[peer] {
		a.m.log.Info("mesh 自动切路：流量超过中转限速的一半，不切到中转", "peer", peer.Short(), "relay", want.Short())
		return
	}
	p.perfVia, p.perfRTT = want, adviceRTT(adv, want)
	st.streak, st.fails, st.trial, st.quiet = 0, 0, want != (Key{}), now.Add(autoCooldown)
	if want == (Key{}) {
		a.m.log.Info("mesh 自动切路回到直连", "peer", peer.Short(), "relay", cur.Short(), "reason", adv.Reason)
	} else {
		a.m.log.Info("mesh 自动切路改经中转", "peer", peer.Short(), "relay", want.Short(), "reason", adv.Reason)
	}
}

func adviceRTT(adv PathAdvice, via Key) time.Duration {
	for _, r := range adv.Paths {
		if r.Via == via {
			return r.Small.P50
		}
	}
	return 0
}

// routed 返回现在经性能中转的对端。
func (a *autoRoute) routed() []healthResult {
	b := a.m.bind
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []healthResult
	for k, p := range b.peers {
		if p.directAlive(now) && b.perfHop(p, now) != nil {
			out = append(out, healthResult{peer: k, via: p.perfVia})
		}
	}
	return out
}

// probe 经各自的中转端到端探测一次（A → R → B → R → A，不只是 A 到 R 通）。本地预算不足不算失败。
func (a *autoRoute) probe(ctx context.Context, probes []healthResult) []healthResult {
	var wg sync.WaitGroup
	for i := range probes {
		wg.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
			defer cancel()
			s := a.m.ProbePath(attempt, probes[i].peer, probes[i].via, pathProbeSmall)
			probes[i].failed = s.Err != nil && !errors.Is(s.Err, ErrProbeLimited) && ctx.Err() == nil
		})
	}
	wg.Wait()
	return probes
}

// healthDone 记下健康检查的结果：连续 autoFailLimit 次失败就回到直连，这条中转暂停使用，进入冷却。
func (a *autoRoute) healthDone(rs []healthResult, now time.Time) {
	b := a.m.bind
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range rs {
		p := b.peers[r.peer]
		if p == nil || p.perfVia != r.via {
			continue
		}
		st := a.stateFor(r.peer)
		if !r.failed {
			st.fails = 0
			continue
		}
		st.fails++
		if st.fails < autoFailLimit {
			continue
		}
		p.perfVia = Key{}
		st.fails, st.trial, st.streak = 0, false, 0
		st.banned[r.via], st.quiet = now.Add(autoBan), now.Add(autoCooldown)
		a.m.log.Warn("mesh 自动切路的中转连续探测不通，回到直连", "peer", r.peer.Short(), "relay", r.via.Short())
	}
}
