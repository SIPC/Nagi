package alert

import (
	"strings"
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

// 告警键按前缀归到事件通知（* 匹配会话 ID 等一段）；没登记的键照常发送；每一项的键唯一、至少一个前缀，
// 两项的前缀不能互相覆盖（否则关掉一项会连带关掉另一项）。
func TestNoticeCatalog(t *testing.T) {
	for key, want := range map[string]string{
		"security:token:1795285737":           "token",
		"security:reset-password:1":           "password",
		"session:abc123:session_reuse":        "session_reuse",
		"session:abc123:session_tls_tool":     "session_tls_tool",
		"event:takeover:p/web":                "takeover",
		"security:admin-removed:1":            "admin_removed",
		"security:admin:1":                    "admin",
		"security:node-panel-removed:1":       "node_removed",
		"security:alert-notification:token:1": "",
		"mem":                                 "",
	} {
		n, ok := NoticeOf(key)
		if want == "" && ok || want != "" && n.Key != want {
			t.Errorf("%s 应归到 %q，实际 %q", key, want, n.Key)
		}
	}
	if NoticeOn(map[string]bool{"session_reuse": false}, "session:x:session_device_changed") != true ||
		NoticeOn(map[string]bool{"session_reuse": false}, "session:x:session_reuse") != false {
		t.Error("关掉一项只影响这一项")
	}
	seen := map[string]bool{}
	for _, n := range Notices {
		if seen[n.Key] || len(n.Prefixes) == 0 || n.Zh == "" || n.En == "" {
			t.Errorf("事件通知 %q 重复或不完整", n.Key)
		}
		seen[n.Key] = true
		for _, p := range n.Prefixes {
			sample := strings.ReplaceAll(p, "*", "x") + "1"
			if got, _ := NoticeOf(sample); got.Key != n.Key {
				t.Errorf("%s 的前缀 %q 被 %s 抢先匹配", n.Key, p, got.Key)
			}
		}
	}
}
