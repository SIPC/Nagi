package alert

import (
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 事件通知（见 docs/monitoring.md#事件通知）：没有阈值、一次性或由面板判断的通知，在 设置 → 告警 里只有开关，
// 默认全开。关掉的记在 AlertPolicy.notifications（只记 false）。发送前按告警的键找到属于哪一项（NoticeOf）。
//
// 规则告警（Defs）不在这里，它们按规则开关。

// 通知的类别。前四类是安全类：只有管理员能改开关。
const (
	NoticeAccount  = "account"  // 账号和凭据
	NoticeSession  = "session"  // 会话和登录
	NoticeCluster  = "cluster"  // 集群和敏感操作
	NoticeSettings = "settings" // 面板设置和审计
	NoticeFailover = "failover" // 节点故障和顶替
)

// Notice 是一项事件通知。
type Notice struct {
	Key      string
	Category string
	// Prefixes 是这项通知的告警键的前缀（面板的通知是 security:<名字>: 等，节点的是 event:<名字>: 等）。
	Prefixes []string
	// Recovers：有恢复通知。
	Recovers bool
	// Risky：关掉以后，攻击者的这类操作不会通知你（界面上标出来，见 docs/monitoring.md#事件通知）。
	Risky bool
	// Node：节点上判断、发出的（随生效规则下发开关）。
	Node bool
	// Zh、En 是什么情况下触发。
	Zh, En string
}

// Security 判断这一项是不是安全类（只有管理员能改开关）。
func (n Notice) Security() bool { return n.Category != NoticeFailover }

// Notices 是全部事件通知，顺序就是界面上的顺序。
var Notices = []Notice{
	// 账号和凭据
	{Key: "2fa_disabled", Category: NoticeAccount, Prefixes: []string{"security:2fa:"}, Risky: true, Zh: "关闭两步验证", En: "Two-factor authentication turned off"},
	{Key: "passkey", Category: NoticeAccount, Prefixes: []string{"security:passkey:"}, Zh: "添加、删除或清除通行密钥", En: "Passkey added, deleted or cleared"},
	{Key: "password", Category: NoticeAccount, Prefixes: []string{"security:password:", "security:reset-password:"}, Zh: "修改自己的密码，或成员的密码被重置", En: "Own password changed or a member's password reset"},
	{Key: "admin", Category: NoticeAccount, Prefixes: []string{"security:admin:"}, Risky: true, Zh: "新增管理员，或成员改成管理员", En: "Administrator added or a member made administrator"},
	{Key: "admin_removed", Category: NoticeAccount, Prefixes: []string{"security:admin-removed:"}, Zh: "取消管理员", En: "Administrator role removed"},
	{Key: "user_deleted", Category: NoticeAccount, Prefixes: []string{"security:user-deleted:"}, Zh: "删除成员", En: "Member deleted"},
	{Key: "role", Category: NoticeAccount, Prefixes: []string{"security:role:"}, Zh: "新建、修改或删除角色", En: "Role created, changed or deleted"},
	{Key: "token", Category: NoticeAccount, Prefixes: []string{"security:token:"}, Risky: true, Zh: "生成访问令牌", En: "Access token created"},
	{Key: "oauth", Category: NoticeAccount, Prefixes: []string{"security:oauth:"}, Zh: "批准 OAuth 授权", En: "OAuth client authorized"},
	{Key: "admin_networks", Category: NoticeAccount, Prefixes: []string{"security:admin-networks:"}, Zh: "修改或取消管理员登录地址", En: "Administrator networks changed or removed"},
	{Key: "entrance", Category: NoticeAccount, Prefixes: []string{"security:entrance:"}, Zh: "修改安全入口", En: "Secure entrance changed"},
	{Key: "session_policy", Category: NoticeAccount, Prefixes: []string{"security:session-policy:"}, Zh: "修改会话策略", En: "Session policy changed"},

	// 会话和登录
	{Key: "login_totp", Category: NoticeSession, Prefixes: []string{"totp:", "login-approval:"}, Zh: "两步验证或手机确认连续失败 5 次", En: "Five failed two-factor or phone approvals in a row"},
	{Key: "login_ratelimit", Category: NoticeSession, Prefixes: []string{"ratelimit:"}, Zh: "登录限速（每小时汇总一次）", En: "Login rate limiting (hourly summary)"},
	{Key: "login_new_network", Category: NoticeSession, Prefixes: []string{"network:"}, Zh: "管理员从新网段登录", En: "Administrator signed in from a new network"},
	{Key: "admin_network_denied", Category: NoticeSession, Prefixes: []string{"admin-network:"}, Zh: "管理员从不允许的地址登录（已拒绝）", En: "Administrator sign-in from a disallowed address (denied)"},
	{Key: "app_attestation", Category: NoticeSession, Prefixes: []string{"app-attestation:"}, Zh: "添加手机时身份校验失败", En: "Phone attestation failed while pairing"},
	{Key: "app_paired", Category: NoticeSession, Prefixes: []string{"app-paired:"}, Zh: "添加手机", En: "Phone paired"},
	{Key: "app_denied", Category: NoticeSession, Prefixes: []string{"login-approval-denied:"}, Zh: "在手机上拒绝登录", En: "Sign-in denied on the phone"},
	{Key: "app_revoked", Category: NoticeSession, Prefixes: []string{"app-revoked:"}, Zh: "手机的证明证书被吊销", En: "Phone attestation certificate revoked"},
	{Key: "session_reuse", Category: NoticeSession, Prefixes: []string{"session:*:session_reuse"}, Risky: true, Zh: "会话令牌被重用（可能被盗用）", En: "Session token reused (possible theft)"},
	{Key: "session_device", Category: NoticeSession, Prefixes: []string{"session:*:session_device_changed"}, Zh: "会话在另一种浏览器或系统上使用", En: "Session used from another browser or OS"},
	{Key: "session_tls_tool", Category: NoticeSession, Prefixes: []string{"session:*:session_tls_tool"}, Zh: "会话被脚本工具使用", En: "Session used by a scripting tool"},
	{Key: "session_network_flip", Category: NoticeSession, Prefixes: []string{"session:*:session_network_flip"}, Zh: "会话的网络反复切换", En: "Session network keeps switching"},
	{Key: "session_key_missing", Category: NoticeSession, Prefixes: []string{"session:*:session_key_missing"}, Zh: "会话拿不出签名密钥", En: "Session missing its signing key"},
	{Key: "session_signature", Category: NoticeSession, Prefixes: []string{"session:*:session_signature_invalid"}, Risky: true, Zh: "会话签名无效", En: "Invalid session signature"},
	{Key: "token_denied", Category: NoticeSession, Prefixes: []string{"token-denied:"}, Zh: "访问令牌在不允许的网络使用", En: "Access token used from a disallowed network"},
	{Key: "token_network", Category: NoticeSession, Prefixes: []string{"token-network:"}, Zh: "高权限令牌首次在新网络使用", En: "Privileged token first used from a new network"},

	// 集群和敏感操作
	{Key: "node_enroll", Category: NoticeCluster, Prefixes: []string{"security:enroll:", "security:enroll-invite:"}, Zh: "节点加入或等待批准", En: "Node joined or awaiting approval"},
	{Key: "node_approve", Category: NoticeCluster, Prefixes: []string{"security:node-approve:"}, Zh: "批准节点加入", En: "Node approved"},
	{Key: "node_removed", Category: NoticeCluster, Prefixes: []string{"security:node-removed:", "security:node-panel-removed:"}, Zh: "删除节点或从节点移除面板", En: "Node deleted or panel removed from a node"},
	{Key: "enroll_command", Category: NoticeCluster, Prefixes: []string{"security:enroll-command:"}, Zh: "生成加入命令或邀请", En: "Join command or invite created"},
	{Key: "node_handover", Category: NoticeCluster, Prefixes: []string{"security:node-handover:"}, Zh: "把节点移交给其他面板", En: "Nodes handed over to another panel"},
	{Key: "standby", Category: NoticeCluster, Prefixes: []string{"security:standby-code:", "security:standby-promote:"}, Zh: "生成备用面板配对码或提升备用面板", En: "Standby pairing code created or standby promoted"},
	{Key: "standby_sync", Category: NoticeCluster, Prefixes: []string{"standby_sync"}, Recovers: true, Zh: "备用面板连不上主面板超过 1 分钟", En: "Standby panel cannot reach the primary for over a minute"},
	{Key: "removed_primary", Category: NoticeCluster, Prefixes: []string{"removed_primary"}, Zh: "已移除的主面板仍在连接节点", En: "A removed primary panel still connects to nodes"},
	{Key: "isolate", Category: NoticeCluster, Prefixes: []string{"security:isolate:"}, Zh: "隔离或解除隔离节点", En: "Node isolated or released"},
	{Key: "violation", Category: NoticeCluster, Prefixes: []string{"violation:", "violation-unknown:"}, Zh: "节点越权（疑似被攻破）", En: "Node violated its permissions (possibly compromised)"},
	{Key: "ha_settings", Category: NoticeCluster, Prefixes: []string{"security:ha-settings:"}, Zh: "修改顶替、备份、节点网络或中转池设置", En: "Failover, backup, node network or relay pool settings changed"},
	{Key: "privileged", Category: NoticeCluster, Prefixes: []string{"security:privileged:"}, Zh: "保存或部署带特权内容的配置", En: "Configuration with privileged content saved or deployed"},
	{Key: "panel_tool", Category: NoticeCluster, Prefixes: []string{"security:panel-admin:"}, Zh: "访问令牌使用管理面板的工具", En: "Access token used a panel administration tool"},
	{Key: "node_shell", Category: NoticeCluster, Prefixes: []string{"node-shell:"}, Risky: true, Zh: "打开节点 root 终端", En: "Node root shell opened"},
	{Key: "node_exec", Category: NoticeCluster, Prefixes: []string{"node-exec:"}, Risky: true, Zh: "在节点上执行命令", En: "Command run on a node"},

	// 面板设置和审计
	{Key: "panel_address", Category: NoticeSettings, Prefixes: []string{"security:panel-address"}, Zh: "修改面板地址（生效前发出）", En: "Panel address changed (sent before it applies)"},
	{Key: "panel_domain", Category: NoticeSettings, Prefixes: []string{"security:panel-domain"}, Zh: "修改面板域名（生效前发出）", En: "Panel domain changed (sent before it applies)"},
	{Key: "alert_config", Category: NoticeSettings, Prefixes: []string{"security:alert-config"}, Risky: true, Zh: "修改告警配置（先发到旧渠道）", En: "Alert configuration changed (sent to the old channels first)"},
	{Key: "audit_sinks", Category: NoticeSettings, Prefixes: []string{"security:audit-sinks:"}, Zh: "修改审计外发目标", En: "Audit forwarding changed"},
	{Key: "audit_retention", Category: NoticeSettings, Prefixes: []string{"security:audit-retention:"}, Zh: "缩短审计保留期", En: "Audit retention shortened"},
	{Key: "recording", Category: NoticeSettings, Prefixes: []string{"security:recording:"}, Zh: "开启或关闭终端录制", En: "Terminal recording turned on or off"},
	{Key: "audit_sink_failing", Category: NoticeSettings, Prefixes: []string{"audit-sink:"}, Recovers: true, Zh: "审计外发持续失败超过 10 分钟", En: "Audit forwarding failing for over 10 minutes"},

	// 节点故障和顶替
	{Key: "replica_index", Category: NoticeFailover, Node: true, Prefixes: []string{"event:replica-index"}, Zh: "副本位置记录无法读取", En: "Replica location index unreadable"},
	{Key: "lease", Category: NoticeFailover, Node: true, Prefixes: []string{"event:lease:"}, Recovers: true, Zh: "续不上顶替租约，容器已停止", En: "Failover lease lost, container stopped"},
	{Key: "takeover", Category: NoticeFailover, Node: true, Prefixes: []string{"event:takeover:"}, Recovers: true, Zh: "顶替失败或等待确认", En: "Takeover failed or awaiting confirmation"},
	{Key: "failback", Category: NoticeFailover, Node: true, Prefixes: []string{"event:failback:"}, Recovers: true, Zh: "切回原节点失败", En: "Failback to the original node failed"},
	{Key: "panel_removed", Category: NoticeFailover, Node: true, Prefixes: []string{"event:panel-removed:"}, Zh: "节点被其他面板移出信任列表", En: "Node removed from another panel's trust list"},
	{Key: "failover_info", Category: NoticeFailover, Node: true, Prefixes: []string{"notice"}, Zh: "已顶替、已切回、已被顶替（信息通知）", En: "Took over, failed back or was taken over (informational)"},
}

// NoticeByKey 返回一项事件通知。
func NoticeByKey(key string) (Notice, bool) {
	for _, n := range Notices {
		if n.Key == key {
			return n, true
		}
	}
	return Notice{}, false
}

// NoticeOf 返回告警键属于哪一项事件通知。前缀里的 * 匹配一段不含冒号的内容（会话 ID）。
func NoticeOf(key string) (Notice, bool) {
	for _, n := range Notices {
		for _, p := range n.Prefixes {
			if matchPrefix(key, p) {
				return n, true
			}
		}
	}
	return Notice{}, false
}

func matchPrefix(key, p string) bool {
	head, tail, wild := strings.Cut(p, "*")
	if !wild {
		return strings.HasPrefix(key, p)
	}
	rest, ok := strings.CutPrefix(key, head)
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, ":")
	return ok && strings.HasPrefix(":"+after, tail)
}

// NoticeOn 判断告警键对应的事件通知开着没有（不属于任何一项的照常发送）。off 是关掉的项（AlertPolicy.notifications）。
func NoticeOn(off map[string]bool, key string) bool {
	n, ok := NoticeOf(key)
	if !ok {
		return true
	}
	v, set := off[n.Key]
	return !set || v
}

// NodeNotices 返回节点要知道的关掉的事件通知（随生效规则下发）。
func NodeNotices(p *nagiv1.AlertPolicy) map[string]bool {
	var out map[string]bool
	for k, v := range p.GetNotifications() {
		if n, ok := NoticeByKey(k); ok && n.Node && !v {
			if out == nil {
				out = map[string]bool{}
			}
			out[k] = false
		}
	}
	return out
}
