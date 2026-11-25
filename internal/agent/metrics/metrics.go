// Package metrics 采集节点和容器的资源指标，存在节点本地 SQLite：
// 最近 24 小时每 10 秒一个点，30 天内每 5 分钟一个点。面板需要时到节点上查询。
//
// 序列名：
//   - node:cpu、node:mem（百分比），node:mem_used、node:disk_free（字节），node:disk（百分比），
//     node:net_rx、node:net_tx（字节/秒）
//   - node:disk@<挂载点>（百分比）：项目目录所在、和节点目录不是同一块的磁盘
//   - c:<项目 ID>/<容器>/<服务>:cpu（百分比）、:mem（字节）、:net_rx、:net_tx（字节/秒）、:restarts（累计次数）
//   - l:<项目 ID>/<容器>:cpu（百分比，每核 100）、:mem（字节）：运行中的容器最多能用多少。
//     设了限制按限制，没设按主机；容器组把各服务的限制加起来，有一个没设或加起来超过主机就按主机
//   - r:<项目 ID>/<规则名>:req、:2xx、:3xx、:4xx、:5xx（请求/秒）、:in、:out（字节/秒）、
//     :lat、:p50、:p95、:p99（耗时，毫秒），由 AddSource 提供
//   - m:<mesh 成员公钥的十六进制>:rx、:tx（字节/秒）：和这个成员之间的流量，由 AddSource 提供
package metrics

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

// Interval 是采样间隔。
const Interval = 10 * time.Second

const (
	bucket     = 5 * time.Minute
	keepRaw    = 24 * time.Hour
	keepM5     = 30 * 24 * time.Hour
	labelLabel = "nagi.project"
)

// Point 是一个时间点的值。
type Point struct {
	Time  time.Time
	Value float64
}

// Collector 定期采集指标。
type Collector struct {
	historyMu       sync.RWMutex // backend handoff; independent of live samples.
	db              *sql.DB
	q               *nodestore.Queries
	docker          *docker.Client
	diskPath        string
	log             *slog.Logger
	budget          *logstore.Store
	historyReadOnly bool
	historyLegacy   *sql.DB // Read-only source until copy/verification finishes.
	// reader is a read-only connection to the separated history (metrics.db),
	// opened on first chart read. db has a single connection shared by sample
	// writes and capacity cleanup; a long chart read on it made cleanup, which
	// holds the journal lock, wait, and every journal write timed out behind it.
	readerMu  sync.Mutex
	reader    *sql.DB
	readerFor *sql.DB

	mu       sync.RWMutex
	latest   map[string]float64
	latestAt time.Time
	disks    []Disk

	prevTime          time.Time
	prevCPUTotal      uint64
	prevCPUIdle       uint64
	prevNetRx         uint64
	prevNetTx         uint64
	prevContainer     map[string]docker.Stats
	lastDownsampledTo time.Time
	sources           []Source
	dropped           atomic.Int64
	// unsaved holds samples not yet written to the chart history, oldest
	// first. They are written together once they span historyFlush; queries
	// merge them, so charts and alert windows still see the latest minute.
	unsavedMu sync.Mutex
	unsaved   []sample

	// ExtraRestarts 返回节点代为重启某个容器的次数（顶替项目由节点代管重启，Docker 不计数）。
	ExtraRestarts func(containerID string) int
	// ProjectDirs 返回所有项目目录。项目目录可能在另一块磁盘上（例如 /srv 挂的数据盘），那块盘也要监控。
	ProjectDirs func() []string
	// OnSample 保存原始采样到统一日志库，不依赖旧指标库成功，也不受旧图表降采样/过期清理影响。
	// 回调在采样协程执行，必须立即返回（只入队），不能修改 values。
	OnSample func(time.Time, map[string]float64)
}

// Source 是额外的指标来源，每次采样时调用，elapsed 是距上次采样的秒数。
type Source func(elapsed float64) map[string]float64

// AddSource 添加额外的指标来源。需要在 Run 之前调用。
func (c *Collector) AddSource(s Source) { c.sources = append(c.sources, s) }

// projectDisks 返回项目目录所在、和节点目录不是同一块的磁盘的使用率，键是挂载点。
func (c *Collector) projectDisks() map[string]float64 {
	if c.ProjectDirs == nil {
		return nil
	}
	out := map[string]float64{}
	seen := map[string]bool{mountOf(c.diskPath): true}
	for _, dir := range c.ProjectDirs() {
		mount := mountOf(dir)
		if mount == "" || seen[mount] {
			continue
		}
		seen[mount] = true
		if total, free, ok := readDisk(mount); ok && total > 0 {
			out[mount] = 100 * float64(total-free) / float64(total)
		}
	}
	return out
}

// readDisks 读取节点上所有本地磁盘的容量和用量，节点目录和项目目录所在的盘一定包含在内。
func (c *Collector) readDisks() []Disk {
	paths := []string{c.diskPath}
	if c.ProjectDirs != nil {
		paths = append(paths, c.ProjectDirs()...)
	}
	var out []Disk
	for _, mount := range selectDisks(readMounts(), paths) {
		if total, free, ok := readDisk(mount); ok && total > 0 {
			out = append(out, Disk{Mount: mount, Total: total, Used: total - free})
		}
	}
	return out
}

// Disks 返回最近一次采样的各磁盘容量和用量。
func (c *Collector) Disks() []Disk {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.disks
}

// DiskFree 返回 path 所在的挂载点和剩余空间（字节）。不支持的系统上 ok 为 false。
func DiskFree(path string) (mount string, free uint64, ok bool) {
	_, free, ok = readDisk(path)
	return mountOf(path), free, ok
}

// Capacity 返回内存总量和 path 所在磁盘的总容量（字节），读不到时为 0。
func Capacity(path string) (memory, disk uint64) {
	if total, _, ok := readMem(); ok {
		memory = total
	}
	if total, _, ok := readDisk(path); ok {
		disk = total
	}
	return memory, disk
}

// New 创建采集器。diskPath 用来计算磁盘使用率（节点目录所在的文件系统）。
func New(db *sql.DB, dc *docker.Client, diskPath string, log *slog.Logger) *Collector {
	return &Collector{
		db: db, q: nodestore.New(db), docker: dc, diskPath: diskPath, log: log,
		latest: map[string]float64{}, prevContainer: map[string]docker.Stats{},
	}
}

// Latest 返回最近一次采样的所有序列的值，供告警判断。
func (c *Collector) Latest() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(c.latest))
	for k, v := range c.latest {
		out[k] = v
	}
	return out
}

// LatestFresh returns no values once sampling has stopped for three intervals.
// A cached sample is not evidence that a fault has recovered.
func (c *Collector) LatestFresh(now time.Time) map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.latestAt.IsZero() || now.Before(c.latestAt) || now.Sub(c.latestAt) > 3*Interval {
		return nil
	}
	out := make(map[string]float64, len(c.latest))
	for k, v := range c.latest {
		if v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			out[k] = v
		}
	}
	return out
}

// Run 按固定的 10 秒节拍采集，直到 ctx 取消。写入在单独的协程里做：
// 磁盘或日志库拥堵时采样照常进行，积压的采样（最多 5 分钟）下一次一起写入；
// 超过上限才丢最旧的并计数，不会把下一次采样往后推
// （v0.37.0 曾因此让图表相邻点间隔超过 50 秒）。
func (c *Collector) Run(ctx context.Context) {
	pending := make(chan sample, pendingSamples)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.writeHistory(ctx, pending)
	}()
	defer func() { <-done }()
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		now := time.Now()
		v := sample{now, c.collect(ctx, now)}
		if c.OnSample != nil {
			c.OnSample(v.at, v.values) // 只入队，不等磁盘
		}
		select {
		case pending <- v:
		default:
			select {
			case <-pending:
				c.dropped.Add(1)
			default:
			}
			pending <- v
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type sample struct {
	at     time.Time
	values map[string]float64
}

// Dropped 返回因写入跟不上采样而没有写进图表历史的采样数（本进程启动以来）。
func (c *Collector) Dropped() int64 { return c.dropped.Load() }

// writeHistory keeps samples in memory and writes them to the chart history
// in one transaction once they span historyFlush, and once more on shutdown.
// Each commit rewrites one B-tree page per series (about 2400 pages on 裸金属),
// however few points it adds: committing every 10 s sample wrote about 2 MB/s
// to disk for 10 KB/s of data.
func (c *Collector) writeHistory(ctx context.Context, pending <-chan sample) {
	for {
		select {
		case <-ctx.Done():
			// Samples still queued were taken before shutdown; keep them too.
			for drained := false; !drained; {
				select {
				case v := <-pending:
					c.keep(v)
				default:
					drained = true
				}
			}
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			c.flushHistory(final)
			cancel()
			// Whatever the final flush could not write is lost; count it.
			c.unsavedMu.Lock()
			if n := len(c.unsaved); n > 0 {
				c.dropped.Add(int64(n))
				c.log.Warn("停止时保存监控数据失败", "samples", n)
				c.unsaved = nil
			}
			c.unsavedMu.Unlock()
			return
		case v := <-pending:
			if c.keep(v) {
				c.flushHistory(ctx)
			}
		}
	}
}

// keep adds a sample to the unsaved ones and reports whether they are due.
func (c *Collector) keep(v sample) bool {
	c.unsavedMu.Lock()
	defer c.unsavedMu.Unlock()
	c.unsaved = append(c.unsaved, v)
	return v.at.Sub(c.unsaved[0].at) >= historyFlush-Interval
}

// flushHistory writes the unsaved samples. They leave memory only after the
// commit, so a concurrent query sees each point in memory or in the database.
// A failed write drops them, as a failed write of one sample did before,
// except when shutdown cancelled it: then they stay for the final flush,
// which writes them without the cancelled context.
func (c *Collector) flushHistory(ctx context.Context) {
	c.unsavedMu.Lock()
	batch := slices.Clone(c.unsaved)
	c.unsavedMu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := c.storeBatch(ctx, batch); err != nil {
		if ctx.Err() != nil {
			return
		}
		c.dropped.Add(int64(len(batch)))
		c.log.Warn("保存监控数据失败", "err", err, "samples", len(batch))
	}
	c.unsavedMu.Lock()
	c.unsaved = slices.Delete(c.unsaved, 0, len(batch))
	c.unsavedMu.Unlock()
	c.maintain(ctx, batch[len(batch)-1].at)
}

// unsavedPoints returns the unsaved points of series starting with prefix in
// [from, to], oldest first, at the second resolution the database stores.
func (c *Collector) unsavedPoints(prefix string, from, to time.Time) map[string][]Point {
	c.unsavedMu.Lock()
	defer c.unsavedMu.Unlock()
	out := map[string][]Point{}
	for _, v := range c.unsaved {
		ts := v.at.Unix()
		if ts < from.Unix() || ts > to.Unix() {
			continue
		}
		for series, value := range v.values {
			if strings.HasPrefix(series, prefix) {
				out[series] = append(out[series], Point{Time: time.Unix(ts, 0), Value: value})
			}
		}
	}
	return out
}

const (
	// historyWait 是图表历史写入等共享日志额度检查的上限；超过就丢掉这批采样并计数。
	historyWait = 2 * time.Second
	// pendingSamples 是等待写入图表历史的采样上限（5 分钟）。
	pendingSamples = 30
	// historyFlush 是采样在内存里攒多久再一起写进图表历史。
	historyFlush = time.Minute
)

func (c *Collector) collect(ctx context.Context, now time.Time) map[string]float64 {
	v := map[string]float64{}
	elapsed := now.Sub(c.prevTime).Seconds()
	first := c.prevTime.IsZero()

	if total, idle, ok := readCPU(); ok {
		if !first && total > c.prevCPUTotal {
			v["node:cpu"] = 100 * (1 - float64(idle-c.prevCPUIdle)/float64(total-c.prevCPUTotal))
		}
		c.prevCPUTotal, c.prevCPUIdle = total, idle
	}
	var memTotal uint64
	if total, avail, ok := readMem(); ok {
		memTotal = total
		v["node:mem"] = 100 * float64(total-avail) / float64(total)
		v["node:mem_used"] = float64(total - avail)
	}
	if total, free, ok := readDisk(c.diskPath); ok && total > 0 {
		v["node:disk"] = 100 * float64(total-free) / float64(total)
		v["node:disk_free"] = float64(free)
	}
	for mount, pct := range c.projectDisks() {
		v["node:disk@"+mount] = pct
	}
	disks := c.readDisks()
	for _, disk := range disks {
		prefix := "node:filesystem@" + disk.Mount + ":"
		v[prefix+"total_bytes"] = float64(disk.Total)
		v[prefix+"used_bytes"] = float64(disk.Used)
		v[prefix+"available_bytes"] = float64(disk.Total - disk.Used)
		if total, free := DiskInodes(disk.Mount); total > 0 && free >= 0 && free <= total {
			v[prefix+"inodes_total"] = float64(total)
			v[prefix+"inodes_free"] = float64(free)
		}
	}
	c.mu.Lock()
	c.disks = disks
	c.mu.Unlock()
	if rx, tx, ok := readNet(); ok {
		if !first && elapsed > 0 && rx >= c.prevNetRx && tx >= c.prevNetTx {
			v["node:net_rx"] = float64(rx-c.prevNetRx) / elapsed
			v["node:net_tx"] = float64(tx-c.prevNetTx) / elapsed
		}
		c.prevNetRx, c.prevNetTx = rx, tx
	}

	containers, err := c.docker.ManagedInstances(ctx, labelLabel)
	if err == nil {
		seen := map[string]bool{}
		limits := map[string]*limitSum{}
		missing := map[string]bool{}
		missingCPU := map[string]bool{}
		for _, ct := range containers {
			prefix := "c:" + ct.Project + "/" + ct.Container + "/" + ct.Service + ":"
			key := "l:" + ct.Project + "/" + ct.Container + ":"
			rt, rtErr := c.docker.Runtime(ctx, ct.ID)
			if rtErr == nil {
				n := rt.RestartCount
				if c.ExtraRestarts != nil {
					n += c.ExtraRestarts(ct.ID)
				}
				v[prefix+"restarts"] += float64(n)
			}
			if ct.State != "running" {
				continue
			}
			st, err := c.docker.ContainerStats(ctx, ct.ID)
			instance := "i:" + ct.Project + "/" + ct.Container + "/" + ct.Service + "/" + ct.ID + ":"
			if err != nil {
				v[instance+"available"] = 0
				missing[key] = true
				continue
			}
			v[instance+"available"] = 1
			for metric, value := range st.Details {
				v[instance+metric] = value
			}
			seen[ct.ID] = true
			v[prefix+"mem"] += float64(st.MemUsed)
			if rtErr == nil {
				if limits[key] == nil {
					limits[key] = &limitSum{}
				}
				limits[key].add(rt, st)
			} else {
				missing[key] = true
			}
			cpuOK := false
			if prev, ok := c.prevContainer[ct.ID]; ok && elapsed > 0 {
				if st.SystemTotal > prev.SystemTotal && st.CPUTotal >= prev.CPUTotal {
					cpus := float64(st.OnlineCPUs)
					if cpus == 0 {
						cpus = 1
					}
					v[prefix+"cpu"] += 100 * float64(st.CPUTotal-prev.CPUTotal) / float64(st.SystemTotal-prev.SystemTotal) * cpus
					cpuOK = true
				}
				if st.NetRx >= prev.NetRx && st.NetTx >= prev.NetTx {
					v[prefix+"net_rx"] += float64(st.NetRx-prev.NetRx) / elapsed
					v[prefix+"net_tx"] += float64(st.NetTx-prev.NetTx) / elapsed
				}
			}
			if !cpuOK {
				missingCPU[key] = true
			}
			c.prevContainer[ct.ID] = st
		}
		for id := range c.prevContainer {
			if !seen[id] {
				delete(c.prevContainer, id)
			}
		}
		for key, l := range limits {
			// Never compare a partial group sum with a partial or full limit.
			if missing[key] {
				continue
			}
			cpu, mem := l.result(memTotal)
			if !missingCPU[key] {
				v[key+"cpu"] = cpu
			}
			v[key+"mem"] = mem
		}
	} else {
		clear(c.prevContainer)
	}

	if !first && elapsed > 0 {
		for _, src := range c.sources {
			for k, x := range src(elapsed) {
				v[k] = x
			}
		}
	}

	c.prevTime = now
	c.mu.Lock()
	c.latest = v
	c.latestAt = now
	c.mu.Unlock()
	return v
}

// limitSum 累加一个容器（容器组里的各服务）的资源限制。
type limitSum struct {
	cpu, mem                   float64 // 设了限制的服务加起来：核数、字节
	cpuUnlimited, memUnlimited bool    // 有服务没设限制
	hostCPUs                   float64 // 主机（容器能用的）核数
	hostMem                    float64 // Docker 报的内存上限：没设限制时就是主机内存，读不到 /proc/meminfo 时备用
}

func (l *limitSum) add(rt docker.Runtime, st docker.Stats) {
	if rt.CPULimit > 0 {
		l.cpu += rt.CPULimit
	} else {
		l.cpuUnlimited = true
	}
	if rt.MemLimit > 0 {
		l.mem += float64(rt.MemLimit)
	} else {
		l.memUnlimited = true
		l.hostMem = max(l.hostMem, float64(st.MemLimit))
	}
	l.hostCPUs = max(l.hostCPUs, float64(st.OnlineCPUs))
}

// result 返回 CPU（百分比，每核 100）和内存（字节）的上限。memTotal 是主机内存，读不到时为 0。
func (l *limitSum) result(memTotal uint64) (cpu, mem float64) {
	hostCPUs := l.hostCPUs
	if hostCPUs == 0 {
		hostCPUs = float64(runtime.NumCPU())
	}
	cpu = l.cpu
	if l.cpuUnlimited || cpu > hostCPUs {
		cpu = hostCPUs
	}
	hostMem := float64(memTotal)
	if hostMem == 0 {
		hostMem = l.hostMem
	}
	mem = l.mem
	if hostMem > 0 && (l.memUnlimited || mem > hostMem) {
		mem = hostMem
	}
	return 100 * cpu, mem
}

func (c *Collector) store(ctx context.Context, now time.Time, values map[string]float64) error {
	return c.storeBatch(ctx, []sample{{now, values}})
}

func (c *Collector) storeBatch(ctx context.Context, batch []sample) error {
	c.historyMu.RLock()
	defer c.historyMu.RUnlock()
	if c.historyReadOnly {
		return errors.New("监控历史迁移未完成，旧业务库只读")
	}
	// 只在额度检查时占用共享日志锁；metrics.db 自己的事务不再持有它。
	if c.budget != nil {
		wait, cancel := context.WithTimeout(ctx, historyWait)
		reserve := int64(64 << 10)
		for _, v := range batch {
			reserve += int64(len(v.values)) * 16384
		}
		err := c.budget.Reserve(wait, 2, min(reserve, 64<<20))
		cancel()
		if err != nil {
			return err
		}
	}
	return c.storeHistory(ctx, batch)
}

// storeHistory 把一批采样放在一个事务里提交。
func (c *Collector) storeHistory(ctx context.Context, batch []sample) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := c.q.WithTx(tx)
	for _, v := range batch {
		ts := v.at.Unix()
		for series, value := range v.values {
			if err := q.InsertMetric(ctx, nodestore.InsertMetricParams{Series: series, Ts: ts, Value: value}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// maintain 把完整的 5 分钟窗口降采样，并清理过期数据。
func (c *Collector) maintain(ctx context.Context, now time.Time) {
	c.historyMu.RLock()
	defer c.historyMu.RUnlock()
	if c.historyReadOnly || c.historyLegacy != nil {
		return
	}
	end := now.Truncate(bucket)
	if c.lastDownsampledTo.IsZero() {
		c.lastDownsampledTo = end
		return
	}
	for c.lastDownsampledTo.Before(end) {
		from := c.lastDownsampledTo
		to := from.Add(bucket)
		rows, err := c.q.AverageRaw(ctx, nodestore.AverageRawParams{FromTs: from.Unix(), ToTs: to.Unix()})
		if err != nil {
			return
		}
		// 一个 5 分钟窗口一次额度检查、一个事务；等额度超过上限时下次再补这个窗口。
		if c.budget != nil {
			wait, cancel := context.WithTimeout(ctx, historyWait)
			err := c.budget.Reserve(wait, 2, int64(len(rows))*256+(64<<10))
			cancel()
			if err != nil {
				return
			}
		}
		if err := c.insertWindow(ctx, from, rows); err != nil {
			return
		}
		c.lastDownsampledTo = to
	}
	// 统一预算接管后历史按容量淘汰，不再另套 24 小时/30 天过期规则。
	if c.budget == nil {
		_ = c.q.DeleteRawBefore(ctx, now.Add(-keepRaw).Unix())
		_ = c.q.DeleteM5Before(ctx, now.Add(-keepM5).Unix())
	}
}

func (c *Collector) insertWindow(ctx context.Context, from time.Time, rows []nodestore.AverageRawRow) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := c.q.WithTx(tx)
	for _, r := range rows {
		v := r.Value
		// 速率序列：窗口里没有采样的时间算 0（见 rateSeries）。
		if want := float64(bucket / Interval); rateSeries(r.Series) && float64(r.Samples) < want {
			v *= float64(r.Samples) / want
		}
		if err := q.InsertM5(ctx, nodestore.InsertM5Params{Series: r.Series, Ts: from.Unix(), Value: v}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rateSeries 报告序列是不是端口映射按每秒速率记的计数：请求、各类状态码、连接、失败、流量。
// 这些序列在有过请求或连接以后才开始记，之前没有采样。合并成一个点时把没有采样的时间当成 0：
// 只平均有采样的部分再乘以时长，映射刚开始有流量的那一段会被放大（例如 1 分钟里的 17 个连接在 10 分钟的点里算成 170 个）。
// 当前连接数（:active）和耗时不是速率，照常取平均。
func rateSeries(name string) bool {
	if !strings.HasPrefix(name, "r:") {
		return false
	}
	switch name[strings.LastIndexByte(name, ':')+1:] {
	case "req", "2xx", "3xx", "4xx", "5xx", "in", "out", "conn", "fail":
		return true
	}
	return false
}

// Window 是一次查询的数据来源：精度（10 秒或 5 分钟）和数据覆盖的时间段 [From, To)。
type Window struct {
	Res      time.Duration
	From, To time.Time
}

// expected 返回从 start 开始、长 step 秒的窗口里应有的采样数。
func (w Window) expected(start, step int64) float64 {
	from, to := max(start, w.From.Unix()), min(start+step, w.To.Unix())
	if to <= from {
		return 0
	}
	return float64(to-from) / w.Res.Seconds()
}

// rawSlack：查询刚好 24 小时（界面的「最近 24 小时」）时请求到节点已经过了一点时间，范围略超过 keepRaw，
// 也用 10 秒精度。否则会用 5 分钟精度，最近不满 5 分钟的部分还没降采样，不算在内。
const rawSlack = time.Minute

// Aggregate 把每条序列按 step 合并：每个窗口一个点，时间取窗口起点。
// 名字以 :p95、:p99 结尾的取窗口内的最大值（耗时的尖峰不被平均掉）；速率序列（rateSeries）按窗口里应有的采样数平均，
// 没有采样的时间算 0；其余取平均值。step 不大于数据精度时原样返回。points 须按时间排序（Query 的结果就是）。
func Aggregate(data map[string][]Point, step time.Duration, w Window) map[string][]Point {
	if step <= w.Res {
		return data
	}
	s := int64(step / time.Second)
	out := make(map[string][]Point, len(data))
	for name, points := range data {
		useMax := strings.HasSuffix(name, ":p95") || strings.HasSuffix(name, ":p99")
		rate := rateSeries(name)
		var res []Point
		var bucket int64
		var sum, peak float64
		n := 0
		flush := func() {
			if n == 0 {
				return
			}
			v := sum / float64(n)
			if useMax {
				v = peak
			} else if want := w.expected(bucket, s); rate && want > float64(n) {
				v = sum / want
			}
			res = append(res, Point{Time: time.Unix(bucket, 0), Value: v})
		}
		for _, p := range points {
			b := p.Time.Unix() / s * s
			if n == 0 || b != bucket {
				flush()
				bucket, sum, peak, n = b, 0, p.Value, 0
			}
			sum += p.Value
			peak = max(peak, p.Value)
			n++
		}
		flush()
		out[name] = res
	}
	return out
}

// Query 查询以 prefix 开头的序列在一段时间内的数据，见 QueryWindow。
func (c *Collector) Query(ctx context.Context, prefix string, from, to time.Time) (map[string][]Point, error) {
	data, _, err := c.QueryWindow(ctx, prefix, from, to)
	return data, err
}

// prefixEnd is the smallest string above every string starting with prefix,
// so the series range uses the (series, ts) primary key instead of scanning the
// ts index of a large legacy table. 0xFF never occurs in UTF-8, so it bounds all.
func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "\xff"
}

// QueryWindow 查询以 prefix 开头的序列在一段时间内的数据，同时返回数据的精度和覆盖的时间段（给 Aggregate 用）。
// 24 小时以内用 10 秒精度，更长用 5 分钟精度。
func (c *Collector) QueryWindow(ctx context.Context, prefix string, from, to time.Time) (map[string][]Point, Window, error) {
	c.historyMu.RLock()
	defer c.historyMu.RUnlock()
	now := time.Now()
	// Taken before reading the database: a point leaves memory only after its
	// commit, so it is either here or in the database read below.
	unsaved := c.unsavedPoints(prefix, from, to)
	out := map[string][]Point{}
	add := func(series string, ts int64, value float64) {
		out[series] = append(out[series], Point{Time: time.Unix(ts, 0), Value: value})
	}
	queries := []*nodestore.Queries{nodestore.New(c.read())}
	if c.historyLegacy != nil {
		queries = append(queries, nodestore.New(c.historyLegacy))
	}
	// While copying, the source remains authoritative for duplicate old points;
	// destination-only points are live samples and must also be visible.
	defer func() {
		if c.historyLegacy == nil {
			return
		}
		for series, points := range out {
			sort.SliceStable(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
			merged := points[:0]
			for _, p := range points {
				if len(merged) > 0 && merged[len(merged)-1].Time.Equal(p.Time) {
					merged[len(merged)-1] = p
				} else {
					merged = append(merged, p)
				}
			}
			out[series] = merged
		}
	}()
	// Match the bounded queries in nodestore. Returning the connection between
	// batches lets reporting and migration writes proceed during large reads.
	const queryBatch = 2048
	if to.Sub(from) <= keepRaw+rawSlack && from.After(now.Add(-keepRaw-rawSlack)) {
		for _, q := range queries {
			params := nodestore.QueryRawParams{Prefix: prefix, PrefixEnd: prefixEnd(prefix), FromTs: from.Unix(), ToTs: to.Unix(), AfterTs: from.Unix() - 1}
			for {
				rows, err := q.QueryRaw(ctx, params)
				if err != nil {
					return nil, Window{}, err
				}
				for _, r := range rows {
					add(r.Series, r.Ts, r.Value)
				}
				if len(rows) < queryBatch {
					break
				}
				last := rows[len(rows)-1]
				params.AfterSeries, params.AfterTs = last.Series, last.Ts
			}
		}
		for series, points := range unsaved {
			have := out[series]
			for _, p := range points {
				// A point committed after unsavedPoints is in both; the legacy
				// merge above sorts and removes duplicates itself.
				if c.historyLegacy != nil || len(have) == 0 || p.Time.After(have[len(have)-1].Time) {
					have = append(have, p)
				}
			}
			out[series] = have
		}
		// 超过 keepRaw 的部分已经删掉了，不算在覆盖的时间段里。
		return out, Window{Res: Interval, From: maxTime(from, now.Add(-keepRaw)), To: minTime(to, now)}, nil
	}
	for _, q := range queries {
		params := nodestore.QueryM5Params{Prefix: prefix, PrefixEnd: prefixEnd(prefix), FromTs: from.Unix(), ToTs: to.Unix(), AfterTs: from.Unix() - 1}
		for {
			rows, err := q.QueryM5(ctx, params)
			if err != nil {
				return nil, Window{}, err
			}
			for _, r := range rows {
				add(r.Series, r.Ts, r.Value)
			}
			if len(rows) < queryBatch {
				break
			}
			last := rows[len(rows)-1]
			params.AfterSeries, params.AfterTs = last.Series, last.Ts
		}
	}
	// 5 分钟的点取窗口起点：覆盖起点不早于 from 的窗口，到 to 所在的窗口为止，且只有已经降采样的
	//（采样最多在内存里攒 historyFlush 才写入并降采样，再留两个采样间隔的余量）。
	first := from.Truncate(bucket)
	if first.Before(from) {
		first = first.Add(bucket)
	}
	return out, Window{Res: bucket, From: first, To: minTime(to.Truncate(bucket).Add(bucket), now.Add(-historyFlush-2*Interval).Truncate(bucket))}, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
