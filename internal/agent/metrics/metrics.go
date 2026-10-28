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
package metrics

import (
	"context"
	"database/sql"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
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
	db       *sql.DB
	q        *nodestore.Queries
	docker   *docker.Client
	diskPath string
	log      *slog.Logger

	mu     sync.RWMutex
	latest map[string]float64

	prevTime          time.Time
	prevCPUTotal      uint64
	prevCPUIdle       uint64
	prevNetRx         uint64
	prevNetTx         uint64
	prevContainer     map[string]docker.Stats
	lastDownsampledTo time.Time
	sources           []Source

	// ExtraRestarts 返回节点代为重启某个容器的次数（顶替项目由节点代管重启，Docker 不计数）。
	ExtraRestarts func(containerID string) int
	// ProjectDirs 返回所有项目目录。项目目录可能在另一块磁盘上（例如 /srv 挂的数据盘），那块盘也要监控。
	ProjectDirs func() []string
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

// DiskFree 返回 path 所在的挂载点和剩余空间（字节）。不支持的系统上 ok 为 false。
func DiskFree(path string) (mount string, free uint64, ok bool) {
	_, free, ok = readDisk(path)
	return mountOf(path), free, ok
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

// Run 每 10 秒采集一次，直到 ctx 取消。
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		now := time.Now()
		values := c.collect(ctx, now)
		if err := c.store(ctx, now, values); err != nil && ctx.Err() == nil {
			c.log.Warn("保存监控数据失败", "err", err)
		}
		c.maintain(ctx, now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

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
		for _, ct := range containers {
			prefix := "c:" + ct.Project + "/" + ct.Container + "/" + ct.Service + ":"
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
			if err != nil {
				continue
			}
			seen[ct.ID] = true
			v[prefix+"mem"] += float64(st.MemUsed)
			if rtErr == nil {
				key := "l:" + ct.Project + "/" + ct.Container + ":"
				if limits[key] == nil {
					limits[key] = &limitSum{}
				}
				limits[key].add(rt, st)
			}
			if prev, ok := c.prevContainer[ct.ID]; ok && elapsed > 0 {
				if st.SystemTotal > prev.SystemTotal && st.CPUTotal >= prev.CPUTotal {
					cpus := float64(st.OnlineCPUs)
					if cpus == 0 {
						cpus = 1
					}
					v[prefix+"cpu"] += 100 * float64(st.CPUTotal-prev.CPUTotal) / float64(st.SystemTotal-prev.SystemTotal) * cpus
				}
				if st.NetRx >= prev.NetRx && st.NetTx >= prev.NetTx {
					v[prefix+"net_rx"] += float64(st.NetRx-prev.NetRx) / elapsed
					v[prefix+"net_tx"] += float64(st.NetTx-prev.NetTx) / elapsed
				}
			}
			c.prevContainer[ct.ID] = st
		}
		for id := range c.prevContainer {
			if !seen[id] {
				delete(c.prevContainer, id)
			}
		}
		for key, l := range limits {
			cpu, mem := l.result(memTotal)
			v[key+"cpu"] = cpu
			v[key+"mem"] = mem
		}
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
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := c.q.WithTx(tx)
	ts := now.Unix()
	for series, value := range values {
		if err := q.InsertMetric(ctx, nodestore.InsertMetricParams{Series: series, Ts: ts, Value: value}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// maintain 把完整的 5 分钟窗口降采样，并清理过期数据。
func (c *Collector) maintain(ctx context.Context, now time.Time) {
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
		for _, r := range rows {
			_ = c.q.InsertM5(ctx, nodestore.InsertM5Params{Series: r.Series, Ts: from.Unix(), Value: r.Value})
		}
		c.lastDownsampledTo = to
	}
	_ = c.q.DeleteRawBefore(ctx, now.Add(-keepRaw).Unix())
	_ = c.q.DeleteM5Before(ctx, now.Add(-keepM5).Unix())
}

// Query 查询以 prefix 开头的序列在一段时间内的数据。24 小时以内用 10 秒精度，更长用 5 分钟精度。
// Aggregate 把每条序列按 step 合并：每个窗口一个点，时间取窗口起点。
// 名字以 :p95、:p99 结尾的取窗口内的最大值（耗时的尖峰不被平均掉），其余取平均值。
// step 不大于采样间隔时原样返回。points 须按时间排序（Query 的结果就是）。
func Aggregate(data map[string][]Point, step time.Duration) map[string][]Point {
	if step <= Interval {
		return data
	}
	s := int64(step / time.Second)
	out := make(map[string][]Point, len(data))
	for name, points := range data {
		useMax := strings.HasSuffix(name, ":p95") || strings.HasSuffix(name, ":p99")
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

func (c *Collector) Query(ctx context.Context, prefix string, from, to time.Time) (map[string][]Point, error) {
	out := map[string][]Point{}
	add := func(series string, ts int64, value float64) {
		out[series] = append(out[series], Point{Time: time.Unix(ts, 0), Value: value})
	}
	if to.Sub(from) <= keepRaw && from.After(time.Now().Add(-keepRaw)) {
		rows, err := c.q.QueryRaw(ctx, nodestore.QueryRawParams{Prefix: prefix, FromTs: from.Unix(), ToTs: to.Unix()})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			add(r.Series, r.Ts, r.Value)
		}
		return out, nil
	}
	rows, err := c.q.QueryM5(ctx, nodestore.QueryM5Params{Prefix: prefix, FromTs: from.Unix(), ToTs: to.Unix()})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		add(r.Series, r.Ts, r.Value)
	}
	return out, nil
}
