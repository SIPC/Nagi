package alert

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestRuleParts(t *testing.T) {
	r := &nagiv1.AlertRules{MemPercent: 80, RestartCount: 4, Enabled: map[string]bool{RuleMem: false, RuleHealth: false}}
	n, c := NodePart(r), ContainerPart(r)
	if n.GetMemPercent() != 80 || n.GetRestartCount() != 0 || Enabled(n, RuleMem) || len(n.GetEnabled()) != 1 {
		t.Errorf("节点部分 %v", n)
	}
	if c.GetRestartCount() != 4 || c.GetMemPercent() != 0 || Enabled(c, RuleHealth) || !Enabled(c, RuleRestart) {
		t.Errorf("容器部分 %v", c)
	}
	if !IsEmpty(nil) || !IsEmpty(&nagiv1.AlertRules{Enabled: map[string]bool{RuleMem: true}}) || IsEmpty(n) {
		t.Error("IsEmpty")
	}
	if CheckRules(&nagiv1.AlertRules{ErrorPercent: 101}) == nil || CheckRules(&nagiv1.AlertRules{Enabled: map[string]bool{"x": true}}) == nil {
		t.Error("CheckRules 应该拒绝")
	}
}
