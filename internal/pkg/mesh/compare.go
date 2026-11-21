package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"time"
)

// 比较的初始阈值（见 docs/mesh.md#比较建议）。尚未在真实网络上验证，只用于给出建议，不驱动切路。
const (
	compareMinSpan     = 30 * time.Second // 至少观察三十秒
	compareMinMeasured = 25               // 每条路线、每种包长至少这么多次真正发出去的往返
	compareLossSlack   = 0.02             // 中转的丢包率最多比直连高 2 个百分点（两种包长分别算）
	compareLossyDirect = 0.05             // 直连丢包达到 5% 才考虑「延迟差不多但更稳」的中转
)

// PathStats 是一条路线一种包长的统计。Measured 只算真正发出去的往返；Lost 是其中超时的；
// Skipped 是本地原因没测成的（预算不足、取消、关闭），不算丢包，也不进分母。
type PathStats struct {
	Measured, Lost, Skipped int
	P50, P90                time.Duration // 成功往返的中位数、90 分位
}

func (s PathStats) loss() float64 {
	if s.Measured == 0 {
		return 1
	}
	return float64(s.Lost) / float64(s.Measured)
}

// PathReport 是一条路线的统计。Reject 非空时这条中转不参与比较。
type PathReport struct {
	Via          Key
	Small, Large PathStats // 256 和 1253 字节
	Reject       string
}

// PathAdvice 是 ComparePaths 的结论。Sufficient 为 false 时证据不够，调用方不能据此改变当前路线
// （包括不能当成「回直连」）；为 true 时 Via 是建议的路线，零值为直连。Paths 中直连在前。
type PathAdvice struct {
	Sufficient bool
	Via        Key
	Reason     string
	Paths      []PathReport
}

// ComparePaths 用 ObservePaths 的一个窗口比较直连和各个中转，只给建议和理由，不切路、不调度。
// 同一窗口、同一包长分别比：丢包率、延迟中位数和 90 分位（稳定性）。中转要入选，两种包长都不能比直连多丢包，
// 并且延迟明显更低、90 分位不变差，或者直连丢包明显而它少一半以上、延迟差不多。
// 中转现在不在中转池、窗口里第一跳变过或者和现在的不一样，都不参与比较。
func (m *Mesh) ComparePaths(peer Key, samples []PathSample) PathAdvice {
	type hopID struct {
		addr  netip.AddrPort
		port  uint16
		epoch uint64
	}
	current := map[Key]*hopID{}
	pooled := map[Key]bool{}
	b := m.bind
	now := time.Now()
	b.mu.Lock()
	p := b.peers[peer]
	for _, s := range samples {
		if _, ok := current[s.Via]; ok || p == nil {
			continue
		}
		hop := p
		if s.Via != (Key{}) {
			hop = b.peers[s.Via]
			pooled[s.Via] = hop.poolRelay()
		}
		current[s.Via] = nil
		if hop != nil && hop.directAlive(now) && b.sock(hop.directConn) != nil {
			port := uint16(b.sock(hop.directConn).LocalAddr().(*net.UDPAddr).Port)
			current[s.Via] = &hopID{hop.direct, port, hop.directEpoch}
		}
	}
	b.mu.Unlock()
	if p == nil {
		return PathAdvice{Reason: "不是成员"}
	}

	var first, last time.Time
	advice := PathAdvice{Paths: []PathReport{{}}} // 直连在前，中转按第一次出现的顺序
	index := map[Key]int{{}: 0}
	rtts := map[Key]map[int][]time.Duration{}
	for _, s := range samples {
		if first.IsZero() || s.Started.Before(first) {
			first = s.Started
		}
		if s.Started.After(last) {
			last = s.Started
		}
		i, ok := index[s.Via]
		if !ok {
			i = len(advice.Paths)
			index[s.Via] = i
			advice.Paths = append(advice.Paths, PathReport{Via: s.Via})
		}
		r := &advice.Paths[i]
		st := &r.Small
		if s.Size == pathProbeLarge {
			st = &r.Large
		} else if s.Size != pathProbeSmall {
			continue
		}
		switch {
		case errors.Is(s.Err, ErrProbePath) && s.Via == (Key{}):
			// 直连没有授权一说：这时直连已经判断为断开（丢包重时会短暂出现），恢复后代次也变了。
			r.Reject = "直连在窗口里断过，要重新观察"
			continue
		case errors.Is(s.Err, ErrProbePath):
			r.Reject = "路线不可用或没有授权"
			continue
		case errors.Is(s.Err, ErrProbeLimited), errors.Is(s.Err, context.Canceled), errors.Is(s.Err, net.ErrClosed):
			st.Skipped++
			continue
		}
		// 真正发出去的往返（成功或超时）都要来自现在这个第一跳，不混入换地址、换 socket 或重新确认之前的结果。
		st.Measured++
		if cur := current[s.Via]; r.Reject == "" && (cur == nil || *cur != (hopID{s.NextHop, s.LocalPort, s.Epoch})) {
			r.Reject = "第一跳在窗口里变过或已经失效，要重新观察"
		}
		if s.Err != nil {
			st.Lost++
			continue
		}
		if rtts[s.Via] == nil {
			rtts[s.Via] = map[int][]time.Duration{}
		}
		rtts[s.Via][s.Size] = append(rtts[s.Via][s.Size], s.RTT)
	}

	for i := range advice.Paths {
		r := &advice.Paths[i]
		r.Small.P50, r.Small.P90 = percentiles(rtts[r.Via][pathProbeSmall])
		r.Large.P50, r.Large.P90 = percentiles(rtts[r.Via][pathProbeLarge])
		if r.Via != (Key{}) && r.Reject == "" && !pooled[r.Via] {
			r.Reject = "不在中转池"
		}
		if r.Reject == "" && (r.Small.Measured < compareMinMeasured || r.Large.Measured < compareMinMeasured) {
			r.Reject = "样本不够"
		}
	}
	direct := advice.Paths[0]
	switch {
	case last.Sub(first) < compareMinSpan:
		advice.Reason = "观察不到三十秒"
		return advice
	case direct.Reject != "":
		advice.Reason = "直连：" + direct.Reject
		return advice
	case direct.Small.Lost == direct.Small.Measured:
		advice.Reason = "直连不通，交给故障兜底，不做性能比较"
		return advice
	}

	d := direct.Small
	margin := max(10*time.Millisecond, d.P50*15/100)
	var best *PathReport
	for i := range advice.Paths[1:] {
		r := &advice.Paths[i+1]
		if r.Reject != "" {
			continue
		}
		s := r.Small
		switch {
		case s.loss() > d.loss()+compareLossSlack:
			r.Reject = "比直连多丢包"
		case r.Large.loss() > direct.Large.loss()+compareLossSlack:
			r.Reject = "大包比直连多丢包（可能是 MTU 不够）"
		case s.P50 <= d.P50-margin && s.P90 <= d.P90:
		case d.loss() >= compareLossyDirect && s.loss() <= d.loss()/2 && s.P50 <= d.P50+margin:
		default:
			r.Reject = "没有比直连明显更好"
		}
		if r.Reject == "" && (best == nil || s.P50 < best.Small.P50) {
			best = r
		}
	}
	advice.Sufficient = true
	if best == nil {
		advice.Reason = "直连最好，没有明显更好的中转"
		return advice
	}
	advice.Via = best.Via
	if best.Small.P50 <= d.P50-margin {
		advice.Reason = "中转延迟明显更低，丢包不比直连多"
	} else {
		advice.Reason = "直连丢包明显，中转丢包少一半以上、延迟差不多"
	}
	return advice
}

// percentiles 返回中位数和 90 分位（最近秩法）；没有样本时为零。
func percentiles(rtts []time.Duration) (p50, p90 time.Duration) {
	if len(rtts) == 0 {
		return 0, 0
	}
	s := slices.Clone(rtts)
	slices.Sort(s)
	at := func(q int) time.Duration { return s[(q*len(s)+99)/100-1] }
	return at(50), at(90)
}
