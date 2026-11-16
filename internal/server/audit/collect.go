package audit

import (
	"context"
	"sync"
)

// MCP 工具调用和它触发的接口调用（见 docs/audit.md#mcp-调用）：工具调用的记录要在工具结束后才知道结果和耗时，
// 而接口调用在工具执行中就记下了。工具执行期间 ctx 里带一个收集器，接口调用的记录先放在这里；工具结束后先写
// 工具调用的记录，再写这些记录，parent_id 指向它。

type collectorKey struct{}

// Collector 收集工具执行期间的记录。
type Collector struct {
	mu     sync.Mutex
	events []Event
}

// WithCollector 返回带收集器的 ctx。
func WithCollector(ctx context.Context) (context.Context, *Collector) {
	c := &Collector{}
	return context.WithValue(ctx, collectorKey{}, c), c
}

func (c *Collector) add(e Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

// RecordWithChildren 先写 parent，再写收集到的记录（parent_id 指向 parent），返回 parent 的 ID。
func (l *Log) RecordWithChildren(ctx context.Context, parent Event, c *Collector) int64 {
	ctx = context.WithValue(ctx, collectorKey{}, (*Collector)(nil))
	id := l.Record(ctx, parent)
	c.mu.Lock()
	events := c.events
	c.events = nil
	c.mu.Unlock()
	for _, e := range events {
		if e.ParentID == 0 {
			e.ParentID = id
		}
		l.Record(ctx, e)
	}
	return id
}
