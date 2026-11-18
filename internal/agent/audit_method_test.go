package agent

import "testing"

// 查询、心跳和轮询类的方法不记进本地审计，修改类的都记。
func TestAuditMethod(t *testing.T) {
	for method, want := range map[string]bool{
		"/nagi.v1.AgentService/UpgradeProgress": false, // 升级期间面板每秒问一次
		"/nagi.v1.AgentService/GetMirrors":      false,
		"/nagi.v1.AgentService/MeshPing":        false,
		"/nagi.v1.AgentService/Upgrade":         true,
		"/nagi.v1.AgentService/CancelUpgrade":   true,
		"/nagi.v1.AgentService/SetAuditSink":    true,
	} {
		if _, got := auditMethod(method); got != want {
			t.Errorf("auditMethod(%s) = %v，应为 %v", method, got, want)
		}
	}
}
