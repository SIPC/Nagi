package nodes

import (
	"sort"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// Changes 通知订阅流（见 docs/android.md#订阅流）「有变化」：节点上报、连上和断开、告警、接口的修改。
// 打开着的流收到后重新查询，和上一次发的比较，变了才发。零值可用。
type Changes struct {
	mu sync.Mutex
	ch chan struct{}
}

// Wait 返回下一次变化时关闭的通道。
func (c *Changes) Wait() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

// Bump 通知有变化。
func (c *Changes) Bump() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch != nil {
		close(c.ch)
		c.ch = nil
	}
}

// ActiveAlert 是一条节点发来、还没恢复的告警。
type ActiveAlert struct {
	NodeID, NodeName     string
	Key, Title           string
	ProjectID, Container string
	Since                time.Time
}

// maxActiveAlertsPerNode 是每个节点最多记几条未恢复的告警。告警来自节点（被攻破的节点可以每次换一个 key），
// 不限的话面板内存会一直涨；满了以后新的 key 不再记，已经记着的照常更新和恢复。
const maxActiveAlertsPerNode = 200

// activeAlerts 记着节点发来、还没恢复的告警（按节点、key）。只在内存里：面板重启后要等节点再发一次
// （节点在 Report 里带上未恢复的告警以后再做，见 docs/android.md#现有接口的改动）。
// 文字和活动记录一样截断（key 200 字、标题 120 字），不存正文。
type activeAlerts struct {
	mu   sync.Mutex
	list map[string]map[string]ActiveAlert // 节点 ID → key → 告警
}

func (a *activeAlerts) record(nodeID, node, projectID, container string, al *nagiv1.Alert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := clipText(al.GetKey(), 200)
	byKey := a.list[nodeID]
	if al.GetStatus() == "resolved" {
		delete(byKey, key)
		if len(byKey) == 0 {
			delete(a.list, nodeID)
		}
		return
	}
	since := time.Now()
	old, ok := byKey[key]
	switch {
	case ok:
		since = old.Since
	case len(byKey) >= maxActiveAlertsPerNode:
		return
	case al.GetTime() != nil:
		since = al.GetTime().AsTime()
	}
	if byKey == nil {
		if a.list == nil {
			a.list = map[string]map[string]ActiveAlert{}
		}
		byKey = map[string]ActiveAlert{}
		a.list[nodeID] = byKey
	}
	byKey[key] = ActiveAlert{NodeID: nodeID, NodeName: clipText(node, 64), Key: key, Title: clipText(al.GetTitle(), 120),
		ProjectID: projectID, Container: container, Since: since}
}

// forget 去掉一个节点的告警（节点被移除时）。
func (a *activeAlerts) forget(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.list, nodeID)
}

// Forget 去掉节点还没恢复的告警（节点被移除、撤销时），并通知订阅流。
func (r *Registry) Forget(nodeID string) {
	r.alerts.forget(nodeID)
	r.Changes.Bump()
}

// ActiveAlerts 返回还没恢复的告警，先出现的在前。
func (r *Registry) ActiveAlerts() []ActiveAlert {
	r.alerts.mu.Lock()
	var out []ActiveAlert
	for _, byKey := range r.alerts.list {
		for _, v := range byKey {
			out = append(out, v)
		}
	}
	r.alerts.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
