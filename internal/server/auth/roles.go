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
	// 访问令牌：只能管自己的，管理员才能看、吊销别人的（处理函数里检查）。
	"ListAccessTokens": true, "CreateAccessToken": true, "RevokeAccessToken": true,
}

// adminReads 是只有管理员能用的查询：内容涉及整个面板或者含有密钥。
var adminReads = map[string]bool{
	"GetAlertConfig": true, "ListInvites": true, "ListActivity": true, "ListUsers": true, "GetNodeMirrors": true, "GetUpdateStatus": true,
	// 节点上的文件（见 docs/node-access.md）：看到的是整个节点。
	"ListNodeDir": true, "ListNodeFileBackups": true,
	// 节点的磁盘明细：涉及节点上所有项目（镜像、容器目录）。
	"GetNodeStorage": true,
}

// operatorReads 是只读角色不能用的查询：含有项目的密钥。
var operatorReads = map[string]bool{
	"GetBackupKey": true, "ListRegistries": true, "ListVolumeFiles": true,
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

// TokenAllowed 判断访问令牌（MCP）能不能调用接口：只能用项目和节点的接口，没勾「可修改」时只能查询
// （见 docs/mcp.md#权限）。角色的限制另由 Allowed 检查。
func TokenAllowed(u User, procedure string) bool {
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 || parts[0] != "nagi.v1.ProjectService" && parts[0] != "nagi.v1.NodeService" {
		return false
	}
	return u.AllowWrite || isRead(parts[1])
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
	if adminReads[method] {
		return false
	}
	if isRead(method) {
		return role == RoleOperator || !operatorReads[method]
	}
	return role == RoleOperator && service == "nagi.v1.ProjectService"
}

// CanOperate 判断角色能不能操作容器（终端、上传文件这些不走 Connect 的入口用）。
func CanOperate(role string) bool { return role == RoleAdmin || role == RoleOperator }
