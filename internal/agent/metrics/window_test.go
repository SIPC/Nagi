package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
)

// 降采样时，速率序列在 5 分钟里没有采样的时间算 0，其他序列照常平均。
// 查询刚好 24 小时（略超过一点）也用 10 秒精度，最近几分钟的数据不会因为还没降采样而缺失。
func TestDownsampleAndWindow(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := New(d, nil, "", nil)

	now := time.Now().Truncate(bucket)
	start := now.Add(-2 * bucket)
	// 上一个 5 分钟窗口只有最后 1 分钟（6 个采样）有连接，每秒 10 个；当前连接数和 CPU 一直有采样。
	for i := range 30 {
		at := start.Add(time.Duration(i) * Interval)
		v := map[string]float64{"r:p/db:active": 2, "node:cpu": 40}
		if i >= 24 {
			v["r:p/db:conn"] = 10
		}
		if err := c.store(ctx, at, v); err != nil {
			t.Fatal(err)
		}
	}
	c.lastDownsampledTo = start
	c.maintain(ctx, start.Add(bucket))
	rows, err := c.q.QueryM5(ctx, nodestore.QueryM5Params{Prefix: "", PrefixEnd: prefixEnd(""), FromTs: start.Unix(), ToTs: start.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.Series] = r.Value
	}
	// 6 个采样 × 每秒 10 个 × 10 秒 = 600 个连接，在 5 分钟里平均每秒 2 个。
	if got["r:p/db:conn"] != 2 || got["r:p/db:active"] != 2 || got["node:cpu"] != 40 {
		t.Fatalf("降采样：%v", got)
	}

	// 「最近 24 小时」的查询到节点时已经略超过 24 小时，仍然用 10 秒精度，覆盖到现在。
	reqNow := time.Now()
	_, w, err := c.QueryWindow(ctx, "r:", reqNow.Add(-24*time.Hour-2*time.Second), reqNow)
	if err != nil {
		t.Fatal(err)
	}
	if w.Res != Interval || w.To.Before(reqNow) {
		t.Fatalf("24 小时的查询应该用 10 秒精度、覆盖到现在：%+v", w)
	}
	// 7 天的查询用 5 分钟精度，只覆盖已经降采样的窗口。
	_, w, err = c.QueryWindow(ctx, "r:", reqNow.Add(-7*24*time.Hour), reqNow)
	if err != nil {
		t.Fatal(err)
	}
	if w.Res != bucket || w.To.After(reqNow.Add(-2*Interval).Truncate(bucket)) || w.From.Before(reqNow.Add(-7*24*time.Hour)) {
		t.Fatalf("7 天的查询：%+v", w)
	}
}

// 前缀和时间范围都必须精确，且每条曲线保持时间顺序。
func TestQueryPrefixRange(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := New(d, nil, "", nil)
	at := time.Now().Add(-time.Minute)
	all := []string{"r", "r:", "r:a", "r:\u9879\u76ee/web:req", "r;", "r;x", "r9", "c:p/web:cpu", "node:cpu", "\u9879"}
	v := map[string]float64{}
	for _, name := range all {
		v[name] = 3
	}
	if err := c.store(ctx, at.Add(Interval), v); err != nil {
		t.Fatal(err)
	}
	for _, name := range all {
		v[name] = 9
	}
	if err := c.store(ctx, at.Add(2*Interval), v); err != nil {
		t.Fatal(err)
	}
	for _, name := range all {
		v[name] = 1
	}
	if err := c.store(ctx, at, v); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"r:", "r", "", "r:\u9879", "\u9879", "node:cpu", "zz"} {
		got, err := c.Query(ctx, prefix, at.Add(-time.Second), at.Add(Interval+time.Second))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range all {
			if _, ok := got[name]; ok != strings.HasPrefix(name, prefix) {
				t.Fatalf("prefix %q: series %q returned=%v", prefix, name, ok)
			}
			if points, ok := got[name]; ok {
				if len(points) != 2 || points[0].Value != 1 || points[1].Value != 3 || !points[0].Time.Equal(time.Unix(at.Unix(), 0)) || !points[1].Time.Equal(time.Unix(at.Add(Interval).Unix(), 0)) {
					t.Fatalf("prefix %q: series %q points=%+v", prefix, name, points)
				}
			}
		}
	}
}

func TestQueryWindowKeepsEveryPointAcrossBatches(t *testing.T) {
	for _, table := range []string{"metrics_raw", "metrics_m5"} {
		t.Run(table, func(t *testing.T) {
			ctx := t.Context()
			d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			at, step := time.Now().Add(-time.Hour).Truncate(Interval), Interval
			if table == "metrics_m5" {
				at, step = at.Add(-48*time.Hour).Truncate(bucket), bucket
			}
			// Multiple series share each timestamp, including a page boundary.
			// The fourth timestamp is outside the inclusive query window.
			_, err = d.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<1030),
				t(j) AS (VALUES(0),(1),(2),(3)) INSERT INTO `+table+`
				SELECT printf('r:%04d:req',i),?+j*?,i+j*10000 FROM n CROSS JOIN t`, at.Unix(), int64(step/time.Second))
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := New(d, nil, "", nil).QueryWindow(ctx, "r:", at, at.Add(2*step))
			if err != nil || len(got) != 1031 {
				t.Fatalf("series=%d err=%v", len(got), err)
			}
			for i := range 1031 {
				series := fmt.Sprintf("r:%04d:req", i)
				points := got[series]
				if len(points) != 3 {
					t.Fatalf("%s: points=%v", series, points)
				}
				for j, point := range points {
					if point.Value != float64(i+j*10000) || !point.Time.Equal(at.Add(time.Duration(j)*step)) {
						t.Fatalf("%s point %d: %+v", series, j, point)
					}
				}
			}
		})
	}
}

// Each commit to the chart history rewrites one page per series, so samples
// are kept in memory and committed once a minute (裸金属 wrote 2 MB/s for
// 10 KB/s of data). Charts must still show every point at once, and stopping
// the node must not lose the ones not yet written.
func TestSamplesCommitOncePerMinuteAndStayQueryable(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "metrics.db")
	legacy, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	history, err := OpenHistory(ctx, path, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	var syncMode, walLimit int
	if err := history.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil || syncMode != 1 {
		t.Fatalf("metrics.db synchronous=%d (want NORMAL=1): %v", syncMode, err)
	}
	if err := history.QueryRow(`PRAGMA journal_size_limit`).Scan(&walLimit); err != nil || walLimit != historyWALLimit {
		t.Fatalf("metrics.db journal_size_limit=%d: %v", walLimit, err)
	}
	c := New(history, nil, "", slog.New(slog.DiscardHandler))
	defer c.CloseReader()
	commits := func() int {
		var n int
		if err := history.QueryRow(`SELECT count(DISTINCT ts) FROM metrics_raw`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	pending := make(chan sample)
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.writeHistory(run, pending) }()
	at := time.Now().Add(-time.Hour).Truncate(time.Minute)
	send := func(i int) {
		pending <- sample{at.Add(time.Duration(i) * Interval), map[string]float64{"node:cpu": float64(i), "c:p/a:mem": 1}}
	}
	for i := range 5 {
		send(i)
	}
	if n := commits(); n != 0 {
		t.Fatalf("%d samples committed before a minute of them was collected", n)
	}
	got, err := c.Query(ctx, "node:", at, at.Add(time.Hour))
	if err != nil || len(got["node:cpu"]) != 5 || got["node:cpu"][4].Value != 4 {
		t.Fatalf("unsaved samples missing from the chart: %+v %v", got, err)
	}
	send(5) // the sixth sample spans the minute
	for deadline := time.Now().Add(5 * time.Second); commits() != 6; {
		if time.Now().After(deadline) {
			t.Fatalf("a minute of samples was not committed: %d", commits())
		}
		time.Sleep(10 * time.Millisecond)
	}
	send(6)
	stop()
	<-done
	got, err = c.Query(ctx, "node:", at, at.Add(time.Hour))
	if err != nil || len(got["node:cpu"]) != 7 || commits() != 7 {
		t.Fatalf("samples lost at shutdown or duplicated: %+v commits=%d %v", got, commits(), err)
	}
	for i, p := range got["node:cpu"] {
		if p.Value != float64(i) || !p.Time.Equal(at.Add(time.Duration(i)*Interval)) {
			t.Fatalf("point %d: %+v", i, p)
		}
	}
}

// Shutdown can cancel a periodic flush that is still waiting for the history
// connection. Its samples must stay for the final flush instead of being
// dropped with the cancelled write (security review of ae787d71: 0 of 6 saved).
func TestShutdownDuringPeriodicFlushKeepsSamples(t *testing.T) {
	ctx := t.Context()
	legacy, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	history, err := OpenHistory(ctx, filepath.Join(t.TempDir(), "metrics.db"), legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	c := New(history, nil, "", slog.New(slog.DiscardHandler))
	defer c.CloseReader()
	conn, err := history.Conn(ctx) // hold the only connection
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pending := make(chan sample, 6)
	at := time.Now().Add(-time.Minute)
	for i := range 6 {
		pending <- sample{at.Add(time.Duration(i) * Interval), map[string]float64{"node:cpu": float64(i)}}
	}
	run, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); c.writeHistory(run, pending) }()
	for deadline := time.Now().Add(5 * time.Second); history.Stats().WaitCount == 0; {
		if time.Now().After(deadline) {
			t.Fatal("periodic flush did not wait for the connection")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	time.Sleep(30 * time.Millisecond) // the cancelled flush returns, the final one starts
	conn.Close()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	var n int
	if err := history.QueryRowContext(ctx, `SELECT count(*) FROM metrics_raw`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 || c.Dropped() != 0 {
		t.Fatalf("shutdown saved %d of 6 samples, dropped %d", n, c.Dropped())
	}
}

func TestLatestFresh(t *testing.T) {
	at := time.Now()
	c := &Collector{latestAt: at, latest: map[string]float64{"node:disk": 42, "bad": math.NaN(), "reset": -1}}
	values := c.LatestFresh(at.Add(3 * Interval))
	if len(values) != 1 || values["node:disk"] != 42 {
		t.Fatalf("invalid values were treated as observations: %v", values)
	}
	values["node:disk"] = 0
	if c.LatestFresh(at)["node:disk"] != 42 {
		t.Fatal("a caller modified the sample cache")
	}
	for _, now := range []time.Time{at.Add(3*Interval + time.Nanosecond), at.Add(-time.Nanosecond)} {
		if len(c.LatestFresh(now)) != 0 {
			t.Fatal("stale or future-dated cache must not prove health")
		}
	}
}

func TestPartialContainerGroupIsNotHealthyEvidence(t *testing.T) {
	var phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := phase.Load()
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.56")
			io.WriteString(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"a","State":"running","Labels":{"nagi.project":"p","nagi.container":"web","com.docker.compose.service":"a"}},{"Id":"b","State":"running","Labels":{"nagi.project":"p","nagi.container":"web","com.docker.compose.service":"b"}}]`)
		case strings.HasSuffix(r.URL.Path, "/json"):
			if n == 5 && strings.Contains(r.URL.Path, "/b/") {
				http.Error(w, "inspect unavailable", 500)
				return
			}
			io.WriteString(w, `{"HostConfig":{"NanoCpus":1000000000,"Memory":1024}}`)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			if n == 2 && strings.Contains(r.URL.Path, "/b/") {
				http.Error(w, "stats unavailable", 500)
				return
			}
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":1},"memory_stats":{"usage":256,"limit":1024}}`, 50+n*50, 100+n*1000)
		default:
			t.Errorf("unexpected Docker endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.56")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	dc, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	c := New(nil, dc, t.TempDir(), nil)
	at := time.Now()
	for n := int32(0); n <= 5; n++ {
		phase.Store(n)
		v := c.collect(context.Background(), at.Add(time.Duration(n)*Interval))
		_, cpuOK := v["l:p/web:cpu"]
		_, memOK := v["l:p/web:mem"]
		wantCPU, wantMem := n == 1 || n == 4, n != 2 && n != 5
		if cpuOK != wantCPU || memOK != wantMem {
			t.Fatalf("phase %d: partial or unavailable samples counted as complete: %v", n, v)
		}
		if cpuOK && (v["l:p/web:cpu"] != 100 || v["c:p/web/a:cpu"]+v["c:p/web/b:cpu"] != 10) {
			t.Fatalf("phase %d: incorrect full group CPU result: %v", n, v)
		}
		if memOK && v["l:p/web:mem"] != 2048 {
			t.Fatalf("phase %d: incorrect full group memory limit: %v", n, v)
		}
	}
}
