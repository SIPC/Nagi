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
	Key, Title, Message  string
	ProjectID, Container string
	Since                time.Time
}

// activeAlerts 记着节点发来、还没恢复的告警（按 节点|key）。只在内存里：面板重启后要等节点再发一次
// （节点在 Report 里带上未恢复的告警以后再做，见 docs/android.md#现有接口的改动）。
type activeAlerts struct {
	mu   sync.Mutex
	list map[string]ActiveAlert
}

func (a *activeAlerts) record(nodeID, node, projectID, container string, al *nagiv1.Alert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.list == nil {
		a.list = map[string]ActiveAlert{}
	}
	k := nodeID + "|" + al.GetKey()
	if al.GetStatus() == "resolved" {
		delete(a.list, k)
		return
	}
	since := time.Now()
	if old, ok := a.list[k]; ok {
		since = old.Since
	} else if t := al.GetTime(); t != nil {
		since = t.AsTime()
	}
	a.list[k] = ActiveAlert{NodeID: nodeID, NodeName: node, Key: al.GetKey(), Title: al.GetTitle(), Message: al.GetMessage(),
		ProjectID: projectID, Container: container, Since: since}
}

// forget 去掉一个节点的告警（节点被移除时）。
func (a *activeAlerts) forget(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.list {
		if v.NodeID == nodeID {
			delete(a.list, k)
		}
	}
}

// ActiveAlerts 返回还没恢复的告警，先出现的在前。
func (r *Registry) ActiveAlerts() []ActiveAlert {
	r.alerts.mu.Lock()
	out := make([]ActiveAlert, 0, len(r.alerts.list))
	for _, v := range r.alerts.list {
		out = append(out, v)
	}
	r.alerts.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
