package agent

import (
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/mesh"
)

// 放行刚撤销的 2 分钟内不算越权；每 10 分钟最多 200 条，超出的只计数。
func TestViolationsLimitAndGrace(t *testing.T) {
	v := newViolations(&agent{})
	k := mesh.PublicKey(randKey())
	v.revoke(k)
	v.add(k, "intranet:connect", "p", "web", 80, "")
	if len(v.pending) != 0 {
		t.Fatal("刚撤销放行的重连不应算越权")
	}
	other := mesh.PublicKey(randKey())
	for range violationLimit + 50 {
		v.add(other, "intranet:connect", "p", "web", 80, "")
	}
	if len(v.pending) != violationLimit || v.dropped != 50 {
		t.Fatalf("记下 %d 条、计数 %d 条", len(v.pending), v.dropped)
	}
}

// 越权先等 30 秒再上报，到时按现在的配置再查：已经允许了（两个节点拿到新配置有先有后）不报，仍然不允许的才报。
func TestViolationsRecheck(t *testing.T) {
	v := newViolations(&agent{})
	k := mesh.PublicKey(randKey())
	allowed := false
	v.addCheck(k, "intranet:connect", "p", "web", 80, "no access to the project", func() bool { return !allowed })
	v.addCheck(k, "intranet:connect", "p", "db", 5432, "no access to the project", func() bool { return true })
	now := time.Now()
	if got := v.ready(now); len(got) != 0 {
		t.Fatalf("没等够 %v 就上报了：%v", violationHold, got)
	}
	allowed = true // 新配置到了，web 的放行生效
	got := v.ready(now.Add(violationHold))
	if len(got) != 1 || got[0].GetContainer() != "db" {
		t.Fatalf("应只上报仍然不允许的 db：%v", got)
	}
	if len(v.pending) != 0 {
		t.Fatalf("上报过的应移出：%d", len(v.pending))
	}
}
