package alerts

import (
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestLoadRules(t *testing.T) {
	e := newEngine(t, nil)
	e.projectName = func(id string) string { return id }
	all := func(string) bool { return true }
	node := judge{e.rules(), all}
	enabled := map[string]bool{"cpu": true}
	start := time.Now()
	latest := map[string]float64{
		"node:cpu":         95,
		"c:p1/web/app:cpu": 120, // 两个服务加起来 190%，限额 200%
		"c:p1/web/db:cpu":  70,
		"l:p1/web:cpu":     200,
		"c:p1/idle/x:cpu":  10,
		"l:p1/idle:cpu":    100,
	}
	run := func(at time.Time, rates map[string]routeRate) map[string]*nagiv1.Alert {
		active := map[string]*nagiv1.Alert{}
		e.judgeLoad(at, latest, rates, node, enabled, active, map[string]bool{})
		return active
	}
	if a := run(start, nil); len(a) != 0 {
		t.Fatalf("刚超过阈值不应该告警：%v", a)
	}
	a := run(start.Add(11*time.Minute), nil)
	if a["cpu"] == nil || a["container_cpu:p1/web"] == nil || a["container_cpu:p1/idle"] != nil {
		t.Fatalf("持续 10 分钟后应该告警节点 CPU 和 web：%v", a)
	}
	// 降下来以后重新计时。
	latest["node:cpu"] = 20
	run(start.Add(12*time.Minute), nil)
	latest["node:cpu"] = 95
	if a := run(start.Add(13*time.Minute), nil); a["cpu"] != nil {
		t.Fatal("CPU 降下来后应该重新计时")
	}
	// 关掉规则不判断。
	enabled["cpu"] = false
	if a := run(start.Add(30*time.Minute), nil); a["cpu"] != nil {
		t.Fatal("规则关掉后不应该告警")
	}

	// 请求量突增：最近 5 分钟每秒 100 个，平均每秒 2 个。
	e.load.baseline = map[string]float64{"p1/api": 2, "p1/quiet": 2}
	a = run(start, map[string]routeRate{
		"p1/api":   {requests: 100 * 300, client: 20000},
		"p1/quiet": {requests: 30 * 300},         // 每秒 30 个，不到 50
		"p1/new":   {requests: 100 * 300},        // 没有基线
		"p1/few":   {requests: 150, client: 150}, // 不到 200 个请求
	})
	if a["traffic:p1/api"] == nil || a["traffic:p1/quiet"] != nil || a["traffic:p1/new"] != nil {
		t.Fatalf("突增判断不对：%v", a)
	}
	if a["4xx:p1/api"] == nil || a["4xx:p1/few"] != nil {
		t.Fatalf("4xx 判断不对：%v", a)
	}
	// 规则所属容器关了 4xx 告警。
	e.RouteRules = func(project, route string) (*nagiv1.AlertRules, bool) {
		return &nagiv1.AlertRules{Enabled: map[string]bool{"4xx": false}}, true
	}
	if a := run(start, map[string]routeRate{"p1/api": {requests: 1000, client: 900}}); a["4xx:p1/api"] != nil {
		t.Fatal("容器关了 4xx 告警，不应该告警")
	}
}
