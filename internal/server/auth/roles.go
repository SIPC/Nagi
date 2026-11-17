package auth

import "strings"

// 角色（见 docs/panel.md#成员和角色）。规则按接口方法判断，没有写明的修改一律只有管理员能做。
const (
	RoleAdmin    = "admin"    // 全部权限
	RoleOperator = "operator" // 管理项目和容器，节点和面板设置只能查看
	RoleViewer   = "viewer"   // 只能查看，看不到配置和密钥
)

// ValidRole 判断是不是合法的角色。
func ValidRole(role string) bool {
	return role == RoleAdmin || role == RoleOperator || role == RoleViewer
}

// selfMethods 是所有角色都能用的：只影响自己的账号。
var selfMethods = map[string]bool{
	"GetSecurity": true, "BeginTOTP": true, "EnableTOTP": true, "DisableTOTP": true,
	"ListSessions": true, "RevokeSession": true,
	// 通行密钥：只能管自己的（见 docs/panel.md#通行密钥）。
	"BeginPasskeyRegistration": true, "AddPasskey": true, "DeletePasskey": true,
	// 访问令牌：只能管自己的，管理员才能看、吊销别人的（处理函数里检查）。
	"ListAccessTokens": true, "CreateAccessToken": true, "RevokeAccessToken": true,
}

// adminReads 是只有管理员能用的查询：内容涉及整个面板或者含有密钥。
var adminReads = map[string]bool{
	"GetAlertConfig": true, "ListInvites": true, "ListActivity": true, "ListUsers": true, "GetNodeMirrors": true, "GetUpdateStatus": true,
	// 节点上的文件（见 docs/node-access.md）：看到的是整个节点。
	"ListNodeDir": true, "ListNodeFileBackups": true, "ListNodeDirSizes": true,
	// 节点的磁盘明细：涉及节点上所有项目（镜像、容器目录）。
	"GetNodeStorage": true,
	// 节点上的镜像（见 docs/agent.md#节点镜像）：涉及节点上所有项目。删除、清理、拉取是 NodeService 的修改，默认只给管理员。
	// 访问令牌按 TokenAllowed 的规则：查看不用额外权限，删除、清理、拉取要勾「可修改」。
	"ListNodeImages": true,
	// 终端录制和审计设置（见 docs/audit.md）。
	"GetRecording": true, "GetAuditSettings": true,
	// 管理员登录地址。
	"GetAdminNetworks": true,
}

// adminWrites 是项目接口里只有管理员能用的修改：项目分组决定成员的访问范围（见 docs/panel.md#分组和标签）。
var adminWrites = map[string]bool{
	"SetProjectGroup": true, "RenameProjectGroup": true,
}

// operatorReads 是只读角色不能用的查询：含有项目的密钥。
var operatorReads = map[string]bool{
	"GetBackupKey": true, "ListRegistries": true, "ListVolumeFiles": true, "ListVolumeDirSizes": true,
}

var readPrefixes = []string{"Get", "List", "Query", "Stream", "Check", "Ping"}

func isRead(method string) bool {
	for _, p := range readPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

// tokenNodeProcedures 是访问令牌要有「节点终端」权限才能调用的接口（见 docs/mcp.md#节点文件和节点级操作），值表示是不是修改。
// 都是网页上要先做高权限确认、或者等于把节点交出去的操作：令牌做不了高权限确认，只有生成（或 OAuth 授权）时做过确认、
// 单独勾了「节点终端」的管理员令牌能用，这时按确认过处理（ElevatedFor）。
var tokenNodeProcedures = map[string]bool{
	// 节点上的文件（见 docs/node-access.md#文件）。
	"/nagi.v1.NodeService/ListNodeDir": false, "/nagi.v1.NodeService/ReadNodeFile": false,
	"/nagi.v1.NodeService/ListNodeFileBackups": false, "/nagi.v1.NodeService/ListNodeDirSizes": false,
	"/nagi.v1.NodeService/WriteNodeFile": true, "/nagi.v1.NodeService/DeleteNodeFile": true, "/nagi.v1.NodeService/CreateNodeDir": true,
	"/nagi.v1.NodeService/RestoreNodeFileBackup": true, "/nagi.v1.NodeService/RenameNodeFile": true,
	"/nagi.v1.NodeService/MoveNodeFiles": true, "/nagi.v1.NodeService/CopyNodeFiles": true,
	"/nagi.v1.NodeService/CompressNodeFiles": true, "/nagi.v1.NodeService/ExtractNodeFile": true,
	// 导入现有目录：读主机上的文件（见 docs/agent.md#现有目录）。
	"/nagi.v1.NodeService/InspectComposeDir": false, "/nagi.v1.ProjectService/ImportComposeDir": true,
	// 隔离节点、恢复从节点缓存拼回的顶替和备份设置。
	"/nagi.v1.NodeService/IsolateNode": true, "/nagi.v1.NodeService/UnisolateNode": true,
	"/nagi.v1.NodeService/ConfirmPendingHA": true,
}

// TokenAllowed 判断访问令牌（MCP）能不能调用接口：只能用项目和节点的接口，没勾「可修改」时只能查询
// （见 docs/mcp.md#权限）；tokenNodeProcedures 里的还要勾「节点终端」。角色的限制另由 Allowed 检查。
func TokenAllowed(u User, procedure string) bool {
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 || parts[0] != "nagi.v1.ProjectService" && parts[0] != "nagi.v1.NodeService" {
		return false
	}
	if write, ok := tokenNodeProcedures[procedure]; ok {
		return u.AllowNodeExec && (u.AllowWrite || !write)
	}
	return u.AllowWrite || isRead(parts[1])
}

// ElevatedFor 判断调用这个接口时算不算做过高权限确认：网页的会话看 Elevated；访问令牌没有高权限确认，
// 只有勾了「节点终端」的管理员令牌调用 tokenNodeProcedures 里的接口时算确认过（生成令牌、OAuth 授权时已经确认过身份，
// 每次调用 MCP 工具还要用户再确认一次，见 docs/mcp.md#节点文件和节点级操作）。
func (u User) ElevatedFor(procedure string) bool {
	if u.TokenID == 0 {
		return u.Elevated()
	}
	_, ok := tokenNodeProcedures[procedure]
	return ok && u.AllowNodeExec && u.Role == RoleAdmin
}

// Allowed 判断角色能不能调用接口（procedure 形如 /nagi.v1.ProjectService/Deploy）。
func Allowed(role, procedure string) bool {
	if role == RoleAdmin {
		return true
	}
	if role != RoleOperator && role != RoleViewer {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	service, method := parts[0], parts[1]
	if service == "nagi.v1.AuthService" || selfMethods[method] {
		return true
	}
	if adminReads[method] || adminWrites[method] {
		return false
	}
	if isRead(method) {
		return role == RoleOperator || !operatorReads[method]
	}
	return role == RoleOperator && service == "nagi.v1.ProjectService"
}

// CanOperate 判断角色能不能操作容器（终端、上传文件这些不走 Connect 的入口用）。
func CanOperate(role string) bool { return role == RoleAdmin || role == RoleOperator }
