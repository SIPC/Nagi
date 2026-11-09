package projects

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 容器单独设置了告警规则就用它的（整组替换），否则用项目的；都没有时下发空规则，不是 nil。
func TestEffectiveAlertRules(t *testing.T) {
	project := &nagiv1.ProjectSettings{AlertRules: &nagiv1.AlertRules{RestartCount: 5}}
	own, _ := proto.Marshal(&nagiv1.AlertRules{Enabled: map[string]bool{"restart": false, "errors": true}})
	if r := effectiveAlertRules(project, store.Container{}); r.GetRestartCount() != 5 {
		t.Errorf("没有单独设置时应该用项目的：%v", r)
	}
	if r := effectiveAlertRules(project, store.Container{AlertRules: own}); r.GetRestartCount() != 0 || r.GetEnabled()["restart"] {
		t.Errorf("单独设置时整组替换项目的：%v", r)
	}
	if r := effectiveAlertRules(nil, store.Container{}); r == nil {
		t.Error("都没有设置时也要下发空规则")
	}
}
