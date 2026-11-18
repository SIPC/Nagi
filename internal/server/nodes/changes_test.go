package nodes

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// M2：节点发来的未恢复告警截断文字、不存正文、每个节点有条数上限，节点被移除后清掉。
func TestActiveAlertsBounded(t *testing.T) {
	r := &Registry{}
	long := strings.Repeat("长", 4096)
	for i := range maxActiveAlertsPerNode + 50 {
		r.alerts.record("n1", "node-1", "", "", &nagiv1.Alert{Key: fmt.Sprintf("k%d-%s", i, long), Title: long, Message: long})
	}
	r.alerts.record("n2", "node-2", "", "", &nagiv1.Alert{Key: "disk", Title: "磁盘快满了"})
	list := r.ActiveAlerts()
	if len(list) != maxActiveAlertsPerNode+1 {
		t.Fatalf("每个节点最多 %d 条，实际共 %d 条", maxActiveAlertsPerNode, len(list))
	}
	for _, a := range list {
		if n := utf8.RuneCountInString(a.Key); n > 201 {
			t.Fatalf("key 应截到 200 字：%d", n)
		}
		if n := utf8.RuneCountInString(a.Title); n > 121 {
			t.Fatalf("标题应截到 120 字：%d", n)
		}
	}

	// 满了以后已经记着的照常恢复，恢复后能记新的。
	first := fmt.Sprintf("k0-%s", long)
	r.alerts.record("n1", "node-1", "", "", &nagiv1.Alert{Key: first, Status: "resolved"})
	r.alerts.record("n1", "node-1", "", "", &nagiv1.Alert{Key: "new"})
	found := false
	for _, a := range r.ActiveAlerts() {
		found = found || a.NodeID == "n1" && a.Key == "new"
	}
	if !found {
		t.Error("恢复一条以后应能记新的")
	}

	// 节点被移除：它的告警都去掉，别的节点的留着。
	wait := r.Changes.Wait()
	r.Forget("n1")
	list = r.ActiveAlerts()
	if len(list) != 1 || list[0].NodeID != "n2" || list[0].Title != "磁盘快满了" {
		t.Errorf("移除节点后应只剩别的节点的告警：%+v", list)
	}
	select {
	case <-wait:
	default:
		t.Error("移除节点后应通知订阅流")
	}
}
