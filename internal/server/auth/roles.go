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
	// 副本的存放位置：在节点上任意位置建目录、写数据（见 docs/snapshot.md#副本的存放位置）。
	"SetBackupReplicaDir": true,
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

// tokenNeed 是访问令牌调用一个接口要勾的选项。
type tokenNeed struct{ write, nodeExec bool }

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
	// 删除、清理节点上的镜像（见 docs/agent.md#节点镜像）：节点拒绝删有容器在用、Nagi 需要的，只要「可修改」。
	"/nagi.v1.NodeService/DeleteNodeImages": needWrite, "/nagi.v1.NodeService/PruneNodeImages": needWrite,
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
	return (!n.write || u.AllowWrite) && (!n.nodeExec || u.AllowNodeExec)
}

// TokenAllowed 判断访问令牌（MCP）能不能调用接口：只能用项目和节点的接口，没勾「可修改」时只能查询
// （见 docs/mcp.md#权限）；tokenElevated 里的按那里要求的选项。角色的限制另由 Allowed 检查。
func TokenAllowed(u User, procedure string) bool {
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 || parts[0] != "nagi.v1.ProjectService" && parts[0] != "nagi.v1.NodeService" {
		return false
	}
	if need, ok := tokenElevated[procedure]; ok {
		return need.metBy(u)
	}
	return u.AllowWrite || isRead(parts[1])
}

// ElevatedFor 判断调用这个接口时算不算做过高权限确认。网页的会话看 Elevated（和以前一样）；访问令牌只在调用
// tokenElevated 里的接口、是管理员、勾了要求的选项时算确认过，别的一律不算。
func (u User) ElevatedFor(procedure string) bool {
	if u.TokenID == 0 {
		return u.Elevated()
	}
	need, ok := tokenElevated[procedure]
	return ok && u.Role == RoleAdmin && need.metBy(u)
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
