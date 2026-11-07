package mesh

import (
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/reedsolomon"
)

// FEC（前向纠错）：直连时每 fecGroup 个 WireGuard 包算出 m 个校验包一起发，路上丢的包由接收方算回来，
// mesh 里的 TCP 察觉不到丢包，也就不降速（见 docs/mesh.md#抗丢包）。m 按对方报告的丢包率调整，丢包很少时为 0，
// 只多一个包头。只对支持 FEC 的对端用（能力包里带着）；经面板中转的包不加。
//
//	数据包：0x10 组号(4) 组内序号(1) 计划的组大小(1，最高位表示这一组有校验包) WireGuard 包
//	校验包：0x11 组号(4) 序号(1，从组大小开始) 实际的组大小(1) 校验包个数(1) 校验分片
//
// 校验分片按"长度(2) 包 补零"对齐到组里最长的包。第一个字节和 WireGuard（1～4）、控制包（'N'）都不重。
//
// 组号从随机值开始：对方重启后，新组号和接收方记着的旧组号几乎不会挨着。收到的组号和最新的差 fecResync 以上时，
// 接收方当作对方重启过（或者断了很久），清空记着的组从头开始；否则新组和旧组同号时新包被当成重复的丢掉，
// 比旧组号小得多时新组一建好就当过期的删掉，校验包也就算不回丢的包。
//
// 有校验包的组按顺序交给 WireGuard：前面缺了包时，后面的包先扣下，等校验包把缺的算回来再一起交，
// 最多等 fecHold。否则后面的包先到，TCP 看到乱序就当成丢包、立刻降速，算回来也晚了。
const (
	fecData   = 0x10
	fecParity = 0x11

	fecDataHeader   = 1 + 4 + 1 + 1
	fecParityHeader = 1 + 4 + 1 + 1 + 1

	fecGroup = 10
	// fecFlush：组没凑满时等这么久就按已有的包发校验，免得流量小的时候丢的包等不到校验。
	fecFlush = 5 * time.Millisecond
	// fecKeep：接收方保留最近这么多组，等迟到的校验包。
	fecKeep = 32
	// fecHold：缺包时后面的包最多扣这么久；fecMaxHeld：最多扣这么多个，再多就放行。
	fecHold    = 15 * time.Millisecond
	fecMaxHeld = 4096
	fecProtect = 0x80
	// fecLossSamples：攒够这么多个包才报一次丢包率（约 2.5 MB）。0.2% 的丢包在几百个包里经常一个都碰不上。
	fecLossSamples = 2000
	fecBurst       = 3
	// fecResync：组号和最新的组差这么多，就不是迟到的包（最多差 fecKeep 组），而是对方重启过或者断了很久。
	fecResync = 1 << 10
)

// parityFor 由丢包率（百万分之几）决定每组加几个校验包。
func parityFor(lossPPM uint32) int {
	switch {
	case lossPPM < 500: // 0.05%
		return 0
	case lossPPM < 5000: // 0.5%
		return 1
	case lossPPM < 20000: // 2%
		return 2
	default:
		return 3
	}
}

var (
	codecMu sync.Mutex
	codecs  = map[[2]int]reedsolomon.Encoder{}
)

func codec(k, m int) (reedsolomon.Encoder, error) {
	codecMu.Lock()
	defer codecMu.Unlock()
	key := [2]int{k, m}
	if c := codecs[key]; c != nil {
		return c, nil
	}
	c, err := reedsolomon.New(k, m)
	if err == nil {
		codecs[key] = c
	}
	return c, err
}

// shard 是对齐后的分片：长度(2) 包 补零。
func shard(pkt []byte, size int) []byte {
	s := make([]byte, size)
	binary.BigEndian.PutUint16(s, uint16(len(pkt)))
	copy(s[2:], pkt)
	return s
}

// ---- 发送 ----

// fecEncoder 是发往一个对端的 FEC 状态。
// newFECEncoder 返回新的编码器，组号从随机值开始（见上文）。
func newFECEncoder(parity int) *fecEncoder {
	return &fecEncoder{parity: parity, gid: rand.Uint32()}
}

type fecEncoder struct {
	mu      sync.Mutex
	parity  int // 每组的校验包个数，由对方报告的丢包率决定，一开始是 1
	gid     uint32
	n       int      // 当前组已经发了几个包
	m       int      // 当前组的校验包个数：一组开始时定下，组内不变
	pkts    [][]byte // 当前组的包，只在有校验包时留着
	started time.Time
	lower   int       // 连续几次报告的丢包率要求更少的校验包
	pause   time.Time // 这之前不加校验包（对方说校验包没用）
	// overhead：FEC 多发的字节数（包头和校验包）。WireGuard 只算它交来的包，这部分要另外算进流量。
	overhead uint64
}

// fecPause：对方说校验包救不回丢的包时，这么久内不加。线路拥堵时成串地丢包，校验包救不回来，
// 反而多占本来就不够的带宽，丢得更多。
const fecPause = time.Minute

// giveUp 在对方说校验包没用时调用：先不加，fecPause 后再按丢包率试。
func (e *fecEncoder) giveUp(now time.Time) {
	e.mu.Lock()
	e.parity, e.lower, e.pause = 0, 0, now.Add(fecPause)
	e.mu.Unlock()
}

func (e *fecEncoder) setParity(m int) {
	e.mu.Lock()
	e.parity = m
	e.mu.Unlock()
}

// fecLowerAfter：丢包率要连续这么多次报告都偏低才减少校验包，增加则立刻生效。丢包少时一次报告里可能正好没丢，
// 一下就减，冗余会来回跳，TCP 跟着降速。
const fecLowerAfter = 3

// adjust 按对方报告的丢包率调整校验包个数。
func (e *fecEncoder) adjust(lossPPM uint32, now time.Time) {
	m := parityFor(lossPPM)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case now.Before(e.pause):
	case m >= e.parity:
		e.parity, e.lower = m, 0
	case e.lower+1 >= fecLowerAfter:
		e.parity, e.lower = m, 0
	default:
		e.lower++
	}
}

func (e *fecEncoder) parityCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.parity
}

// frame 给一个数据包加上 FEC 包头，返回要发出去的数据包，以及凑满一组时要接着发的校验包。
// 一组的第一个包发出时，安排 fecFlush 后调用 later，组没凑满时由它发校验包。
func (e *fecEncoder) frame(pkt []byte, now time.Time, later func()) (data []byte, parity [][]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == 0 {
		e.started, e.m = now, e.parity
		if e.m > 0 {
			time.AfterFunc(fecFlush, later)
		}
	}
	data = make([]byte, fecDataHeader+len(pkt))
	data[0] = fecData
	binary.BigEndian.PutUint32(data[1:], e.gid)
	data[5] = byte(e.n)
	data[6] = fecGroup
	if e.m > 0 {
		data[6] |= fecProtect
	}
	copy(data[fecDataHeader:], pkt)
	e.overhead += fecDataHeader
	if e.m > 0 {
		e.pkts = append(e.pkts, data[fecDataHeader:])
	}
	e.n++
	if e.n >= fecGroup {
		parity = e.finish()
	}
	return data, parity
}

// flush 在组没凑满、又等了 fecFlush 时发校验包。没有校验包的组不提前结束：接收方只能从校验包里知道一组提前结束了，
// 没有校验包又提前结束，它会一直等后面的包、把它们算成丢包。
func (e *fecEncoder) flush(now time.Time) [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == 0 || e.m == 0 || now.Sub(e.started) < fecFlush {
		return nil
	}
	return e.finish()
}

// finish 结束当前组：算出校验包，组号加一。调用时持有 e.mu。
func (e *fecEncoder) finish() [][]byte {
	k, m := e.n, e.m
	pkts := e.pkts
	gid := e.gid
	e.pkts, e.n, e.gid = nil, 0, e.gid+1
	if m == 0 {
		return nil
	}
	enc, err := codec(k, m)
	if err != nil {
		return nil
	}
	size := 0
	for _, p := range pkts {
		size = max(size, len(p))
	}
	size += 2
	shards := make([][]byte, k+m)
	for i, p := range pkts {
		shards[i] = shard(p, size)
	}
	for i := k; i < k+m; i++ {
		shards[i] = make([]byte, size)
	}
	if enc.Encode(shards) != nil {
		return nil
	}
	out := make([][]byte, m)
	for i := 0; i < m; i++ {
		b := make([]byte, fecParityHeader+size)
		b[0] = fecParity
		binary.BigEndian.PutUint32(b[1:], gid)
		b[5] = byte(k + i)
		b[6] = byte(k)
		b[7] = byte(m)
		copy(b[fecParityHeader:], shards[k+i])
		out[i] = b
		e.overhead += uint64(len(b))
	}
	return out
}

// overheadBytes 返回 FEC 到现在多发的字节数。
func (e *fecEncoder) overheadBytes() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.overhead
}

// ---- 接收 ----

type fecGroupState struct {
	k, m      int // 实际的组大小、校验包个数；收到校验包之前 k 是计划的组大小，m 为 0
	shards    [][]byte
	have      int // 收到的数据包个数
	recovered bool
}

// seq 是一个数据包在发送顺序里的位置。
type seq struct {
	gid uint32
	idx int
}

func (a seq) before(b seq) bool {
	if a.gid != b.gid {
		return int32(a.gid-b.gid) < 0
	}
	return a.idx < b.idx
}

// fecDecoder 是来自一个对端的 FEC 状态，同时统计丢包率报给对方。
type fecDecoder struct {
	mu     sync.Mutex
	groups map[uint32]*fecGroupState
	newest uint32
	seen   bool
	// 按顺序交付：next 是下一个该交的包，held 是先到、扣下的包，heldAt 是开始扣的时间。
	next    seq
	ordered bool
	held    map[seq][]byte
	heldAt  time.Time
	timer   atomic.Bool // 已经安排了 release
	// 统计窗口：已经结束的组里应该收到、实际收到的数据包个数（不算恢复出来的）。
	expected, received uint64
	// 丢了的包里，有多少是一组里丢了 fecBurst 个以上的：成串地丢多半是线路拥堵，校验包救不回来。
	lost, burstLost uint64
	recovered       uint64
	// overhead：收到的 FEC 包的字节数减去交给 WireGuard 的字节数：包头、校验包、来晚了的重复包，减去恢复出来的包
	// （WireGuard 算了，线上没有）。加进流量里才和网卡上的一致。
	overhead int64
}

func newFECDecoder() *fecDecoder {
	return &fecDecoder{groups: map[uint32]*fecGroupState{}, held: map[seq][]byte{}}
}

// push 处理一个 FEC 包（数据包或校验包），返回可以按顺序交给 WireGuard 的包。
// 返回 waiting 时还有扣着的包，调用方要在 fecHold 后调用 release。
func (d *fecDecoder) push(pkt []byte, now time.Time) (out [][]byte, waiting bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.overhead += int64(len(pkt))
	if len(pkt) >= 5 && d.seen {
		if diff := int32(binary.BigEndian.Uint32(pkt[1:]) - d.newest); diff >= fecResync || diff <= -fecResync {
			out = d.resync(out)
		}
	}
	switch pkt[0] {
	case fecData:
		s, payload, protected := d.data(pkt)
		if payload == nil {
			return nil, len(d.held) > 0
		}
		d.overhead -= int64(len(payload))
		if !protected {
			return [][]byte{payload}, len(d.held) > 0
		}
		out = d.order(s, payload, out)
	case fecParity:
		gid, rec := d.parity(pkt)
		for idx, p := range rec {
			d.overhead -= int64(len(p))
			out = d.order(seq{gid, idx}, p, out)
		}
	}
	out = d.drain(out)
	if len(d.held) > fecMaxHeld {
		out = d.skip(out)
	}
	if len(d.held) == 0 {
		d.heldAt = time.Time{}
	} else if d.heldAt.IsZero() {
		d.heldAt = now
	}
	return out, len(d.held) > 0
}

// release 放行扣了 fecHold 还等不到的包：跳过缺的包，从最早扣着的接着交。
func (d *fecDecoder) release(now time.Time) (out [][]byte, waiting bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.held) > 0 && now.Sub(d.heldAt) >= fecHold {
		out = d.skip(out)
		d.heldAt = time.Time{}
		if len(d.held) > 0 {
			d.heldAt = now
		}
	}
	return out, len(d.held) > 0
}

// order 交付或扣下一个包。调用时持有 d.mu。
func (d *fecDecoder) order(s seq, p []byte, out [][]byte) [][]byte {
	if !d.ordered {
		// 第一次：从这一组里第一个还没收到的包开始（前面可能丢了，也可能是开 FEC 之前已经交过的）。
		i := 0
		if g := d.groups[s.gid]; g != nil {
			for i < s.idx && g.shards[i] != nil {
				i++
			}
		}
		d.next, d.ordered = seq{s.gid, i}, true
	}
	switch {
	case s.before(d.next): // 已经跳过去了（来得太晚），直接交
		return append(out, p)
	case s == d.next:
		out = append(out, p)
		d.advance()
		return out
	default:
		d.held[s] = p
		return out
	}
}

// drain 交出接在 next 后面、已经到了的包。调用时持有 d.mu。
func (d *fecDecoder) drain(out [][]byte) [][]byte {
	for {
		p, ok := d.held[d.next]
		if !ok {
			// 一组提前结束（校验包里的实际组大小比计划的小）时直接跳到下一组。
			if g := d.groups[d.next.gid]; g != nil && g.m > 0 && d.next.idx >= g.k {
				d.next = seq{d.next.gid + 1, 0}
				continue
			}
			return out
		}
		delete(d.held, d.next)
		out = append(out, p)
		d.advance()
	}
}

// skip 跳到最早扣着的包，从那里接着交。调用时持有 d.mu。
func (d *fecDecoder) skip(out [][]byte) [][]byte {
	first, ok := seq{}, false
	for s := range d.held {
		if !ok || s.before(first) {
			first, ok = s, true
		}
	}
	if !ok {
		return out
	}
	d.next = first
	return d.drain(out)
}

func (d *fecDecoder) advance() {
	d.next.idx++
	size := fecGroup
	if g := d.groups[d.next.gid]; g != nil && g.m > 0 {
		size = g.k
	}
	if d.next.idx >= size {
		d.next = seq{d.next.gid + 1, 0}
	}
}

// resync 清空记着的组，从下一个包重新开始（见上文）。扣着的包按顺序交出去：断了很久时它们是真的包；
// 对方重启时它们属于旧的 WireGuard 会话，WireGuard 自己会丢掉。调用时持有 d.mu。
func (d *fecDecoder) resync(out [][]byte) [][]byte {
	held := make([]seq, 0, len(d.held))
	for s := range d.held {
		held = append(held, s)
	}
	slices.SortFunc(held, func(a, b seq) int {
		if a.before(b) {
			return -1
		}
		if b.before(a) {
			return 1
		}
		return 0
	})
	for _, s := range held {
		out = append(out, d.held[s])
	}
	d.groups, d.held = map[uint32]*fecGroupState{}, map[seq][]byte{}
	d.heldAt, d.seen, d.ordered = time.Time{}, false, false
	return out
}

func (d *fecDecoder) group(gid uint32, k int) *fecGroupState {
	g := d.groups[gid]
	if g == nil {
		g = &fecGroupState{k: k, shards: make([][]byte, k+3)}
		d.groups[gid] = g
		if !d.seen || int32(gid-d.newest) > 0 {
			d.newest, d.seen = gid, true
		}
		d.expire()
	}
	return g
}

// expire 丢掉太旧的组，把它们记进丢包统计。调用时持有 d.mu。
func (d *fecDecoder) expire() {
	for id, g := range d.groups {
		if int32(d.newest-id) >= fecKeep {
			d.count(g)
			delete(d.groups, id)
		}
	}
}

// count 把一个已经结束的组记进丢包统计。组大小按校验包里的实际大小；没收到校验包时按计划的大小：
// 不加校验包时组总会凑满（不会提前结束），加了校验包又全丢了时会多算一点，宁可多加冗余。
func (d *fecDecoder) count(g *fecGroupState) {
	if size := g.k; size > 0 {
		d.expected += uint64(size)
		d.received += uint64(min(g.have, size))
		if n := uint64(size - min(g.have, size)); n > 0 {
			d.lost += n
			if n >= fecBurst {
				d.burstLost += n
			}
		}
	}
}

// data 记下一个数据包，返回它的位置、里面的 WireGuard 包（复制出来的）和这一组有没有校验包。调用时持有 d.mu。
func (d *fecDecoder) data(pkt []byte) (s seq, payload []byte, protected bool) {
	if len(pkt) < fecDataHeader {
		return s, nil, false
	}
	gid, idx, k := binary.BigEndian.Uint32(pkt[1:]), int(pkt[5]), int(pkt[6]&^fecProtect)
	if k == 0 || k > fecGroup || idx >= k {
		return s, nil, false
	}
	protected = pkt[6]&fecProtect != 0
	g := d.group(gid, k)
	if g.shards[idx] != nil {
		return s, nil, false // 重复的包
	}
	// 有校验包的组要留着包（恢复时要用，也可能先扣下）；没有的只记下收到了，不复制。
	payload = pkt[fecDataHeader:]
	if protected {
		payload = append([]byte(nil), payload...)
		g.shards[idx] = payload
	} else {
		g.shards[idx] = gotPacket
	}
	g.have++
	return seq{gid, idx}, payload, protected
}

// gotPacket 标记没有校验包的组里收到了的包。
var gotPacket = []byte{}

// parity 记下一个校验包，返回它的组号和恢复出来的包（组内序号 → 包）。调用时持有 d.mu。
func (d *fecDecoder) parity(pkt []byte) (uint32, map[int][]byte) {
	if len(pkt) < fecParityHeader+2 {
		return 0, nil
	}
	gid, idx, k, m := binary.BigEndian.Uint32(pkt[1:]), int(pkt[5]), int(pkt[6]), int(pkt[7])
	if k == 0 || k > fecGroup || m == 0 || m > 3 || idx < k || idx >= k+m {
		return 0, nil
	}
	g := d.group(gid, k)
	if g.m == 0 {
		g.k, g.m = k, m
	}
	if g.recovered || g.k != k || g.m != m || g.shards[idx] != nil {
		return 0, nil
	}
	g.shards[idx] = append([]byte(nil), pkt[fecParityHeader:]...)
	if g.have >= k {
		return 0, nil
	}
	got := 0
	for _, s := range g.shards[:k+m] {
		if s != nil {
			got++
		}
	}
	if got < k {
		return 0, nil
	}
	return gid, d.reconstruct(g)
}

// reconstruct 用校验包算回丢掉的数据包（组内序号 → 包）。调用时持有 d.mu。
func (d *fecDecoder) reconstruct(g *fecGroupState) map[int][]byte {
	k, m := g.k, g.m
	size := 0
	for i := k; i < k+m; i++ {
		if g.shards[i] != nil {
			size = len(g.shards[i])
			break
		}
	}
	shards := make([][]byte, k+m)
	missing := []int{}
	for i := 0; i < k+m; i++ {
		switch {
		case g.shards[i] == nil:
			if i < k {
				missing = append(missing, i)
			}
		case i < k:
			if len(g.shards[i])+2 > size {
				return nil
			}
			shards[i] = shard(g.shards[i], size)
		default:
			if len(g.shards[i]) != size {
				return nil
			}
			shards[i] = g.shards[i]
		}
	}
	enc, err := codec(k, m)
	if err != nil || enc.ReconstructData(shards) != nil {
		return nil
	}
	g.recovered = true
	out := map[int][]byte{}
	for _, i := range missing {
		n := int(binary.BigEndian.Uint16(shards[i]))
		if n+2 > len(shards[i]) {
			continue
		}
		g.shards[i] = shards[i][2 : 2+n]
		out[i] = g.shards[i]
		d.recovered++
	}
	return out
}

// overheadBytes 返回收到的 FEC 包比交给 WireGuard 的多出的字节数，见 fecDecoder.overhead。
func (d *fecDecoder) overheadBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.overhead
}

func (d *fecDecoder) recoveredCount() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recovered
}

// lossPPM 返回上次调用以来的丢包率（百万分之几），并开始新的统计窗口。样本太少时返回 ok=false。
// useless 表示丢的包大多是成串丢的（一半以上在一组丢 fecBurst 个以上的组里）：多半是线路拥堵，
// 校验包救不回来，还多占本来就不够的带宽。随机丢包时一组丢 3 个以上很少见（丢包 2% 时约千分之一）。
func (d *fecDecoder) lossPPM() (ppm uint32, ok, useless bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.expected < fecLossSamples {
		return 0, false, false
	}
	lost := d.expected - d.received
	ppm = uint32(lost * 1_000_000 / d.expected)
	useless = d.lost >= 20 && d.burstLost*2 > d.lost
	d.expected, d.received, d.lost, d.burstLost = 0, 0, 0, 0
	return ppm, true, useless
}
