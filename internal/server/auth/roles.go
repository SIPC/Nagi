package auth

import "strings"

// 角色（见 docs/panel.md#成员和角色）。成员的权限在 perms.go，这里的名称是预置角色的 builtin 标记，也是给旧版本客户端看的
// 角色名（LegacyRole）。users.role 只有两种值：admin（管理员）和 member（其余的，权限看 role_id）。
const (
	RoleAdmin    = "admin"    // 全部权限
	RoleOperator = "operator" // 预置的操作员
	RoleViewer   = "viewer"   // 预置的只读
	RoleMember   = "member"   // users.role 里非管理员的值
)

// BuiltinRoleID 把旧版本的角色名换成预置角色的 ID，不认识时返回 0。
func BuiltinRoleID(role string) int64 {
	switch role {
	case RoleAdmin:
		return AdminRoleID
	case RoleOperator:
		return OperatorRoleID
	case RoleViewer:
		return ViewerRoleID
	}
	return 0
}

var readPrefixes = []string{"Get", "List", "Query", "Stream", "Check", "Ping", "Watch"}

func isRead(method string) bool {
	for _, p := range readPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

// tokenNeed 是访问令牌调用一个接口要勾的选项。
type tokenNeed struct{ write, nodeExec, panel bool }

var (
	needRead  = tokenNeed{nodeExec: true}
	needNode  = tokenNeed{write: true, nodeExec: true}
	needWrite = tokenNeed{write: true}
)

// tokenElevated 是网页上要先做高权限确认、访问令牌（MCP）也能调用的接口，和令牌要勾的选项（见 docs/mcp.md#高权限确认）。
// 令牌做不了交互式的确认：只有管理员的令牌、勾了这里要求的选项才能调用，调用时算作确认过（ElevatedFor）。
// 「节点终端」只能在做过高权限确认后勾选（生成令牌、OAuth 授权页点允许），MCP 工具每次修改前还要用户确认预览。
// 不在这里的要确认的接口（设置里的生成令牌、两步验证、通行密钥、回放录制、管理员登录地址、测试外送），令牌一律不算确认过。
var tokenElevated = map[string]tokenNeed{
	// 节点上的文件（见 docs/node-access.md#文件）：等于节点的 root，要「节点终端」，修改还要「可修改」。
	"/nagi.v1.NodeService/ListNodeDir": needRead, "/nagi.v1.NodeService/ReadNodeFile": needRead,
	"/nagi.v1.NodeService/ListNodeFileBackups": needRead, "/nagi.v1.NodeService/ListNodeDirSizes": needRead,
	"/nagi.v1.NodeService/WriteNodeFile": needNode, "/nagi.v1.NodeService/DeleteNodeFile": needNode, "/nagi.v1.NodeService/CreateNodeDir": needNode,
	"/nagi.v1.NodeService/RestoreNodeFileBackup": needNode, "/nagi.v1.NodeService/RenameNodeFile": needNode,
	"/nagi.v1.NodeService/MoveNodeFiles": needNode, "/nagi.v1.NodeService/CopyNodeFiles": needNode,
	"/nagi.v1.NodeService/CompressNodeFiles": needNode, "/nagi.v1.NodeService/ExtractNodeFile": needNode,
	// 导入现有目录：读主机上的文件（见 docs/agent.md#现有目录）。
	"/nagi.v1.NodeService/InspectComposeDir": needRead, "/nagi.v1.ProjectService/ImportComposeDir": needNode,
	// 隔离节点、恢复从节点缓存拼回的顶替和备份设置。
	"/nagi.v1.NodeService/IsolateNode": needNode, "/nagi.v1.NodeService/UnisolateNode": needNode,
	"/nagi.v1.NodeService/ConfirmPendingHA": needNode,
	// 副本的存放位置：让节点以 root 在任意位置建目录、写数据（见 docs/snapshot.md#副本的存放位置）。
	"/nagi.v1.NodeService/SetNodeReplicaDir": needNode, "/nagi.v1.ProjectService/SetBackupReplicaDir": needNode,
	// 项目根目录：同样是让节点以 root 在指定位置建目录、写数据。
	"/nagi.v1.NodeService/SetNodeProjectsRoot": needNode,
	// 删除、清理节点上的镜像（见 docs/agent.md#节点镜像）：节点拒绝删有容器在用、Nagi 需要的，只要「可修改」。
	"/nagi.v1.NodeService/DeleteNodeImages": needWrite, "/nagi.v1.NodeService/PruneNodeImages": needWrite,
}

// passwordElevation 是只认密码或验证码做的高权限确认的接口：给自己留下登录凭据的操作，手机的确认密钥不算
// （见 docs/android.md#高权限确认和重新验证）。这些接口也都在 api 的 elevationRequired 里。
var passwordElevation = map[string]bool{
	"/nagi.v1.SettingsService/CreateAccessToken":        true,
	"/nagi.v1.SettingsService/BeginPasskeyRegistration": true,
	"/nagi.v1.SettingsService/BeginTOTP":                true,
	"/nagi.v1.AuthService/CreateAppPairing":             true,
	// 备用面板的配对码会交出主密钥和数据库；移交节点、提升备用面板会让别的面板接管所有节点。
	"/nagi.v1.SettingsService/CreateStandbyCode": true,
	"/nagi.v1.NodeService/HandoverNodes":         true,
	"/nagi.v1.SettingsService/PromoteStandby":    true,
	// 新建成员、修改成员：新建一个自己知道密码的管理员、把成员改成管理员，同样是给自己留下登录凭据。
	"/nagi.v1.SettingsService/CreateUser": true,
	"/nagi.v1.SettingsService/UpdateUser": true,
	// 新建、修改角色：给成员在用的角色加权限，和改成员的角色是一回事。成员和角色的修改统一只认密码。
	"/nagi.v1.SettingsService/CreateRole": true,
	"/nagi.v1.SettingsService/UpdateRole": true,
}

// PasswordElevationProcedures 列出 passwordElevation 里的接口（测试用）。
func PasswordElevationProcedures() []string {
	out := make([]string, 0, len(passwordElevation))
	for p := range passwordElevation {
		out = append(out, p)
	}
	return out
}

// TokenElevatedProcedures 列出 tokenElevated 里的接口（测试用：和 api 的 elevationRequired 对照）。
func TokenElevatedProcedures() []string {
	out := make([]string, 0, len(tokenElevated))
	for p := range tokenElevated {
		out = append(out, p)
	}
	return out
}

func (n tokenNeed) metBy(u User) bool {
	return (!n.write || u.AllowWrite) && (!n.nodeExec || u.AllowNodeExec) && (!n.panel || u.AllowPanel)
}

var (
	panelRead  = tokenNeed{panel: true}
	panelWrite = tokenNeed{panel: true, write: true}
)

// tokenAccount 是项目和节点以外、访问令牌能调用的接口（见 docs/mcp.md#管理面板），其余的设置、账号接口令牌一律不能调用。
// 管理面板（「管理面板」选项，要角色有面板的权限）：面板设置（不含安全入口）、告警渠道、活动和审计记录、面板更新、备用面板的状态（只读）、
// 成员（只读）。一直不开放：生成和吊销令牌、密码、两步验证、通行密钥、会话、OAuth 授权、初始化、高权限确认、安全入口、
// 备用面板的配对码和提升、邀请、成员的角色和访问范围、会话策略、审计设置和外送、管理员登录地址、终端录制。
// 测试 docs/mcp.md#管理面板 列出的每个接口（tools_test.go 的 TestTokenNeverReaches）。
var tokenAccount = map[string]tokenNeed{
	// 自己的项目页排序：只影响自己的账号。修改仍要「可修改」。
	"/nagi.v1.AuthService/GetProjectOrder": {}, "/nagi.v1.AuthService/SetProjectOrder": {write: true},
	"/nagi.v1.SettingsService/GetSettings": panelRead, "/nagi.v1.SettingsService/UpdateSettings": panelWrite,
	"/nagi.v1.SettingsService/SetPanelDomain": panelWrite,
	"/nagi.v1.SettingsService/GetAlertConfig": panelRead, "/nagi.v1.SettingsService/UpdateAlertConfig": panelWrite,
	"/nagi.v1.SettingsService/TestAlertChannel": panelWrite,
	"/nagi.v1.SettingsService/ListActivity":     panelRead, "/nagi.v1.SettingsService/ListAuditEvents": panelRead,
	"/nagi.v1.SettingsService/GetAuditEvent": panelRead, "/nagi.v1.SettingsService/ExportAuditEvents": panelRead,
	"/nagi.v1.SettingsService/VerifyAudit":      panelRead,
	"/nagi.v1.SettingsService/CheckPanelUpdate": panelRead, "/nagi.v1.SettingsService/GetPanelReleaseNotes": panelRead,
	"/nagi.v1.SettingsService/GetUpdateStatus": panelRead, "/nagi.v1.SettingsService/UpdatePanel": panelWrite,
	"/nagi.v1.SettingsService/GetStandbys": panelRead,
	"/nagi.v1.SettingsService/ListUsers":   panelRead,
	// 告警规则：读取不用额外选项（只返回能管的部分），修改要「可修改」。
	"/nagi.v1.AlertService/GetAlertRules": {}, "/nagi.v1.AlertService/UpdateAlertPolicy": {write: true},
	"/nagi.v1.AlertService/SetAlertOverride": {write: true},
}

// tokenScopeAdmin：项目分组和节点标签决定成员的访问范围，属于面板管理，令牌要勾「管理面板」和「可修改」
// （2026-11-19-87c02b5#15）。不要高权限确认，所以不在 tokenElevated 里。
var tokenScopeAdmin = map[string]tokenNeed{
	"/nagi.v1.ProjectService/SetProjectGroup": panelWrite, "/nagi.v1.NodeService/SetNodeTags": panelWrite,
}

// TokenAllowed 判断访问令牌（MCP）能不能调用接口：项目和节点的接口，没勾「可修改」时只能查询（见 docs/mcp.md#权限），
// tokenElevated 里的按那里要求的选项；别的服务只有 tokenAccount 里的。角色的限制另由 Allowed 检查。
func TokenAllowed(u User, procedure string) bool {
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	// 只认密码确认的接口令牌一律算不上确认过，直接拒绝（例如 NodeService 的 HandoverNodes）。
	if passwordElevation[procedure] {
		return false
	}
	if parts[0] != "nagi.v1.ProjectService" && parts[0] != "nagi.v1.NodeService" {
		need, ok := tokenAccount[procedure]
		return ok && need.metBy(u)
	}
	if need, ok := tokenElevated[procedure]; ok {
		return need.metBy(u)
	}
	if need, ok := tokenScopeAdmin[procedure]; ok {
		return need.metBy(u)
	}
	return u.AllowWrite || isRead(parts[1])
}

// ElevatedFor 判断调用这个接口时算不算做过高权限确认。网页的会话看 Elevated（和以前一样）；访问令牌只在调用
// tokenElevated 里的接口、勾了要求的选项时算确认过（选项只在角色有相应权限时生效，见 tokens.go），别的一律不算。
func (u User) ElevatedFor(procedure string) bool {
	if passwordElevation[procedure] && u.ElevatedBy != ElevatedByPassword {
		return false
	}
	if u.TokenID == 0 {
		return u.Elevated()
	}
	need, ok := tokenElevated[procedure]
	return ok && need.metBy(u) && Allowed(u, procedure)
}
