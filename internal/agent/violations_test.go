package agent

import (
	"testing"

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
