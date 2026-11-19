package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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
	rows, err := c.q.QueryM5(ctx, nodestore.QueryM5Params{Prefix: "", FromTs: start.Unix(), ToTs: start.Unix()})
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
