package agent

import (
	"context"
	"net/netip"
	"testing"

	"github.com/SIPC/nagi/internal/agent/project"
)

// G1：Docker 事件流断开期间收不到容器的变化，不用缓存，每次都查；两个事件流都恢复以后再缓存。
func TestEndpointCacheStreamDown(t *testing.T) {
	ctx := context.Background()
	calls := 0
	n := &intranet{}
	n.endpoint = func(context.Context, string, string) (project.Endpoint, bool) {
		calls++
		return project.Endpoint{IP: netip.MustParseAddr("10.0.0.2")}, true
	}
	lookup := func() bool {
		_, _, cached := n.lookupEndpoint(ctx, "p", "db")
		return cached
	}
	lookup()
	if !lookup() || calls != 1 {
		t.Fatalf("平时应该用缓存：查了 %d 次", calls)
	}
	// 两个事件流先后断开：缓存清空并停用。
	n.endpointEvents(false)
	n.endpointEvents(false)
	if lookup() || lookup() || calls != 3 {
		t.Fatalf("事件流断开时不该用缓存：查了 %d 次", calls)
	}
	// 只恢复了一个：仍不用缓存。
	n.endpointEvents(true)
	if lookup() || lookup() || calls != 5 {
		t.Fatalf("还有事件流断开着，不该用缓存：查了 %d 次", calls)
	}
	// 都恢复了：重新缓存。
	n.endpointEvents(true)
	lookup()
	if !lookup() || calls != 6 {
		t.Fatalf("事件流恢复以后应该用缓存：查了 %d 次", calls)
	}
}
