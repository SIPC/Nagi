// Package metrics 采集节点和容器的资源指标，存在节点本地 SQLite：
// 最近 24 小时每 10 秒一个点，30 天内每 5 分钟一个点。面板需要时到节点上查询。
//
// 序列名：
//   - node:cpu、node:mem（百分比），node:mem_used、node:disk_free（字节），node:disk（百分比），
//     node:net_rx、node:net_tx（字节/秒）
//   - c:<项目 ID>/<服务>:cpu（百分比）、:mem（字节）、:net_rx、:net_tx（字节/秒）、:restarts（累计次数）
package metrics

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
)

const (
	interval   = 10 * time.Second
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
	t := time.NewTicker(interval)
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
	if total, avail, ok := readMem(); ok {
		v["node:mem"] = 100 * float64(total-avail) / float64(total)
		v["node:mem_used"] = float64(total - avail)
	}
	if total, free, ok := readDisk(c.diskPath); ok && total > 0 {
		v["node:disk"] = 100 * float64(total-free) / float64(total)
		v["node:disk_free"] = float64(free)
	}
	if rx, tx, ok := readNet(); ok {
		if !first && elapsed > 0 && rx >= c.prevNetRx && tx >= c.prevNetTx {
			v["node:net_rx"] = float64(rx-c.prevNetRx) / elapsed
			v["node:net_tx"] = float64(tx-c.prevNetTx) / elapsed
		}
		c.prevNetRx, c.prevNetTx = rx, tx
	}

	containers, err := c.docker.ManagedContainers(ctx, labelLabel)
	if err == nil {
		seen := map[string]bool{}
		for _, ct := range containers {
			prefix := "c:" + ct.Project + "/" + ct.Service + ":"
			if n, err := c.docker.RestartCount(ctx, ct.ID); err == nil {
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
	}

	c.prevTime = now
	c.mu.Lock()
	c.latest = v
	c.mu.Unlock()
	return v
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
