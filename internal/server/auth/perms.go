package auth

import (
	"slices"
	"strings"
)

// 权限（见 docs/panel.md#成员和角色）：成员选一个角色，角色勾选权限。管理员是固定角色，拥有全部权限。
// 权限不按项目区分：一个成员的权限对他访问范围内的所有项目都一样。
//
// 每个 Connect 接口都在 procedurePerms 里登记到一项权限（或者 publicProcedures、selfProcedures、adminProcedures），
// 没登记的一律拒绝（perms_test.go 列出 api.proto 的全部接口）。没有权限时：接口拒绝，回复里不返回相应的字段
// （api 的 clean.go），界面隐藏对应的入口。

// Perm 是一项权限。名称（permNames）存进数据库、返回给网页和 App，不能改。
type Perm uint8

const (
	// 项目
	PermView          Perm = iota // 查看：状态、事件、监控，是其他项目权限的前提
	PermConfig                    // 查看配置：compose、表单、变量名、版本
	PermLogs                      // 日志
	PermControl                   // 控制：启停重启容器和服务、检查镜像
	PermDeploy                    // 部署：改配置、添加删除容器、部署、回滚、共享变量
	PermTerminal                  // 终端
	PermFilesRead                 // 文件查看
	PermFilesWrite                // 文件修改
	PermSnapshots                 // 快照（含定时快照）
	PermBackup                    // 备份：备份节点、同步、从备份恢复、备份密钥
	PermHA                        // 高可用：顶替、见证、确认接管
	PermMigrate                   // 迁移：迁移、到别处重新部署
	PermNetwork                   // 网络和域名：端口映射、域名、证书、项目连接、内部端口、出站限制
	PermAddresses                 // 查看访问地址：端口映射的 IP:端口、域名期望解析到的 IP、直连下载
	PermAlerts                    // 告警规则
	PermRegistries                // 镜像仓库
	PermProjectManage             // 项目管理：新建、删除、项目设置
	PermPrivileged                // 特权配置：特权 compose、进入特权容器
	// 节点
	PermNodeView    // 查看节点
	PermNodeDetails // 查看节点详情：主机名、系统、版本、路径、挂载点
	PermNodeManage  // 节点管理
	PermNodeImages  // 节点镜像
	PermNodeRoot    // 节点终端和文件（等同 root）
	// 面板
	PermMembers  // 成员和角色
	PermSettings // 面板设置和告警渠道
	PermAudit    // 审计日志：审计页，查询、导出、校验全部记录，录制回放
	PermSecurity // 安全设置
	permCount
)

var permNames = [permCount]string{
	PermView: "project.view", PermConfig: "project.config", PermLogs: "project.logs", PermControl: "project.control",
	PermDeploy: "project.deploy", PermTerminal: "project.terminal", PermFilesRead: "project.files.read",
	PermFilesWrite: "project.files.write", PermSnapshots: "project.snapshots", PermBackup: "project.backup",
	PermHA: "project.ha", PermMigrate: "project.migrate", PermNetwork: "project.network", PermAddresses: "project.addresses",
	PermAlerts: "project.alerts", PermRegistries: "project.registries", PermProjectManage: "project.manage",
	PermPrivileged: "project.privileged",
	PermNodeView:   "node.view", PermNodeDetails: "node.details", PermNodeManage: "node.manage", PermNodeImages: "node.images",
	PermNodeRoot: "node.root",
	PermMembers:  "panel.members", PermSettings: "panel.settings", PermAudit: "panel.audit", PermSecurity: "panel.security",
}

func (p Perm) String() string {
	if p < permCount {
		return permNames[p]
	}
	return ""
}

// Perms 是一组权限。
type Perms uint64

// AllPerms 是全部权限（管理员）。
const AllPerms = Perms(1)<<permCount - 1

func PermsOf(ps ...Perm) Perms {
	var out Perms
	for _, p := range ps {
		out |= 1 << p
	}
	return out
}

func (s Perms) Has(p Perm) bool { return p < permCount && s&(1<<p) != 0 }

// Names 返回权限的名称，按定义的顺序。
func (s Perms) Names() []string {
	out := []string{}
	for p := range permCount {
		if s.Has(p) {
			out = append(out, permNames[p])
		}
	}
	return out
}

// ParsePerms 把名称换成权限，不认识的名称忽略（以后删掉的权限、旧数据）。
func ParsePerms(names []string) Perms {
	var out Perms
	for _, n := range names {
		if i := slices.Index(permNames[:], strings.TrimSpace(n)); i >= 0 {
			out |= 1 << Perm(i)
		}
	}
	return out
}

// ValidPermName 判断是不是认识的权限名称。
func ValidPermName(name string) bool { return slices.Contains(permNames[:], name) }

// PermNames 返回全部权限的名称（测试和界面用）。
func PermNames() []string { return append([]string(nil), permNames[:]...) }

// 预置角色（见 docs/panel.md#成员和角色）。管理员是固定的，操作员、只读可以编辑，这里是新建面板和升级时的默认值，
// 和迁移 0027_roles.sql 里写入的一致（store 的测试核对）。
var (
	PresetOperator = PermsOf(PermView, PermConfig, PermLogs, PermControl, PermDeploy, PermTerminal, PermFilesRead,
		PermFilesWrite, PermSnapshots, PermNodeView)
	PresetViewer = PermsOf(PermView, PermLogs, PermNodeView)
)

// 预置角色的 ID 和 builtin 标记（roles 表）。
const (
	AdminRoleID    int64 = 1
	OperatorRoleID int64 = 2
	ViewerRoleID   int64 = 3
)

// AdminOnlyPerms 暂时只给管理员：自定义角色和预置角色都不能勾（安全设置）。检查的位置都已经按权限写好。
// 审计日志（PermAudit）可以授予：预置的操作员、只读默认没有（见 docs/panel.md#成员和角色）。
var AdminOnlyPerms = PermsOf(PermSecurity)

// projectPerms 是项目的权限：勾了其中任何一项都要有「查看」。
var projectPerms = PermsOf(PermView, PermConfig, PermLogs, PermControl, PermDeploy, PermTerminal, PermFilesRead, PermFilesWrite,
	PermSnapshots, PermBackup, PermHA, PermMigrate, PermNetwork, PermAddresses, PermAlerts, PermRegistries, PermProjectManage, PermPrivileged)

var nodePerms = PermsOf(PermNodeView, PermNodeDetails, PermNodeManage, PermNodeImages, PermNodeRoot)

// permRequires 是一项权限的前提：勾了它就自动带上前提。
var permRequires = map[Perm]Perms{
	PermDeploy:     PermsOf(PermConfig),
	PermPrivileged: PermsOf(PermConfig, PermDeploy),
	PermFilesWrite: PermsOf(PermFilesRead),
	PermAddresses:  PermsOf(PermNetwork),
	// 节点管理要看版本（升级）、路径（项目根目录、副本目录），节点终端和文件本来就看得到整台机器。
	PermNodeManage: PermsOf(PermNodeDetails),
	PermNodeRoot:   PermsOf(PermNodeDetails),
}

// Normalize 补上前提的权限（见 permRequires），去掉只给管理员的。
func (s Perms) Normalize() Perms {
	s &= AllPerms &^ AdminOnlyPerms
	for range 3 {
		for p, need := range permRequires {
			if s.Has(p) {
				s |= need
			}
		}
		if s&projectPerms != 0 {
			s |= PermsOf(PermView)
		}
		if s&nodePerms != 0 {
			s |= PermsOf(PermNodeView)
		}
	}
	return s
}

// legacyRole 是给旧版本客户端（App 的 role 字段）看的角色名：有任何修改类的权限算操作员，否则算只读。
func legacyRole(s Perms) string {
	if s&^PermsOf(PermView, PermConfig, PermLogs, PermFilesRead, PermNodeView, PermNodeDetails, PermAddresses) != 0 {
		return RoleOperator
	}
	return RoleViewer
}

// LegacyRole 返回成员的角色名（admin、operator、viewer），给旧版本的客户端用。
func LegacyRole(admin bool, s Perms) string {
	if admin {
		return RoleAdmin
	}
	return legacyRole(s)
}

// 接口的登记。
//
// publicProcedures：不用登录（初始化、登录、App 配对和换取令牌），由登录检查决定放不放行；登录后也能调用。
// selfProcedures：所有成员都能用，只影响自己的账号，或者回复按权限清理（GetSettings、GetStandbys 等）。
// adminProcedures：只给管理员，不对应任何可以勾选的权限：内容含面板和节点的地址、指纹，或者涉及多个面板。
var publicProcedures = map[string]bool{
	"/nagi.v1.SetupService/GetSetupState": true, "/nagi.v1.SetupService/Setup": true,
	"/nagi.v1.AuthService/Login": true, "/nagi.v1.AuthService/BeginPasskeyLogin": true, "/nagi.v1.AuthService/PasskeyLogin": true,
	// 登录的第二步改用手机确认（见 approval.go）：先核对密码，凭发起时给的凭据取结果。
	"/nagi.v1.AuthService/BeginLoginApproval": true, "/nagi.v1.AuthService/CompleteLoginApproval": true,
	"/nagi.v1.AppService/GetPairing": true, "/nagi.v1.AppService/Pair": true, "/nagi.v1.AppService/Challenge": true,
	"/nagi.v1.AppService/Token": true,
	// 节点用接入码加入面板（enroll.proto），接入码自己就是凭据。
	"/nagi.v1.EnrollService/Enroll": true,
}

var selfProcedures = map[string]bool{
	"/nagi.v1.AuthService/Logout": true, "/nagi.v1.AuthService/Me": true, "/nagi.v1.AuthService/Elevate": true,
	"/nagi.v1.AuthService/ChangePassword": true, "/nagi.v1.AuthService/SetLanguage": true, "/nagi.v1.AuthService/UpdateProfile": true,
	"/nagi.v1.AuthService/GetOAuthRequest": true, "/nagi.v1.AuthService/ApproveOAuth": true,
	"/nagi.v1.AuthService/RegisterSessionKey": true, "/nagi.v1.AuthService/RotateSession": true, "/nagi.v1.AuthService/Reverify": true,
	"/nagi.v1.AuthService/GetProjectOrder": true, "/nagi.v1.AuthService/SetProjectOrder": true,
	"/nagi.v1.AuthService/GetNodeOrder": true, "/nagi.v1.AuthService/SetNodeOrder": true,
	"/nagi.v1.AuthService/CreateAppPairing": true, "/nagi.v1.AuthService/GetAppPairing": true,
	"/nagi.v1.AppService/Info": true, "/nagi.v1.AppService/Logout": true,
	// 手机确认（见 approval.go）：网页发起自己的高权限确认；手机查看、确认自己账号的请求，做自己的高权限确认。
	"/nagi.v1.AuthService/BeginPhoneApproval": true, "/nagi.v1.AuthService/GetPhoneApproval": true,
	"/nagi.v1.AppService/ListApprovals": true, "/nagi.v1.AppService/Approve": true, "/nagi.v1.AppService/Elevate": true,
	// 回复按权限清理（api 的 clean.go）：非管理员只拿到名称、版本，和绑了域名的备用面板。
	"/nagi.v1.SettingsService/GetSettings": true, "/nagi.v1.SettingsService/GetStandbys": true,
	// 审计查询：有「审计日志」时查全部；只有「查看」时只能查访问范围内一个项目的告警（项目设置里出站限制列出被记录的目标），
	// 范围和清理规则和项目的事件一样；别的一律拒绝。处理函数里检查（api 的 audit_api.go）。
	"/nagi.v1.SettingsService/ListAuditEvents": true,
	// 版本号所有人都能看；force（让面板联网检查）只给管理员，处理函数里检查。
	"/nagi.v1.SettingsService/CheckPanelUpdate": true, "/nagi.v1.SettingsService/GetPanelReleaseNotes": true,
	// 自己的两步验证、通行密钥、登录设备和访问令牌。看、吊销别人的令牌只给管理员（处理函数里检查）。
	"/nagi.v1.SettingsService/GetSecurity": true, "/nagi.v1.SettingsService/BeginTOTP": true,
	"/nagi.v1.SettingsService/EnableTOTP": true, "/nagi.v1.SettingsService/DisableTOTP": true,
	"/nagi.v1.SettingsService/ListSessions": true, "/nagi.v1.SettingsService/RevokeSession": true,
	"/nagi.v1.SettingsService/ListAccessTokens": true, "/nagi.v1.SettingsService/CreateAccessToken": true,
	"/nagi.v1.SettingsService/RevokeAccessToken":        true,
	"/nagi.v1.SettingsService/BeginPasskeyRegistration": true, "/nagi.v1.SettingsService/AddPasskey": true,
	"/nagi.v1.SettingsService/DeletePasskey": true,
	// 告警规则（设置 → 告警）：按规则所在的对象要不同的权限（面板设置、节点管理、项目的告警规则或项目管理），
	// 处理函数里检查，读取时只返回能管的部分（api 的 alert_rules.go）。
	"/nagi.v1.AlertService/GetAlertRules": true, "/nagi.v1.AlertService/UpdateAlertPolicy": true,
	"/nagi.v1.AlertService/SetAlertOverride": true, "/nagi.v1.AlertService/SetAlertNotification": true,
	// 基线进度：和当前告警一样按告警权限和访问范围过滤（api 的 alert_baselines.go）。
	"/nagi.v1.AlertService/ListAlertBaselines": true,
}

var adminProcedures = map[string]bool{
	// 一键清理告警：会结束所有项目和节点的当前告警（见 docs/monitoring.md#日志页中的当前告警与人工处理）。
	"/nagi.v1.AlertService/ClearAlerts": true,
	// Reports combine cross-project host and alert evidence.
	"/nagi.v1.ModelReportService/GetReportConfig":    true,
	"/nagi.v1.ModelReportService/UpdateReportConfig": true,
	"/nagi.v1.ModelReportService/GenerateReport":     true,
	"/nagi.v1.ModelReportService/ListReports":        true,
	"/nagi.v1.ModelReportService/GetReport":          true,
	// 混合主机/审计/容器日志暂不向项目成员开放，避免跨项目正文泄露。
	"/nagi.v1.NodeLogService/QueryNodeLogs": true,
	// 节点接入码和安装命令里有面板的地址和指纹。
	"/nagi.v1.SettingsService/CreateInvite": true, "/nagi.v1.SettingsService/ListInvites": true,
	"/nagi.v1.SettingsService/RevokeInvite": true, "/nagi.v1.NodeService/CreateEnrollCommand": true,
	// 面板的对外地址。
	"/nagi.v1.SettingsService/UpdateSettings": true,
	// 备用面板（配对码里有地址和指纹）。
	"/nagi.v1.SettingsService/CreateStandbyCode": true, "/nagi.v1.SettingsService/RemoveStandby": true,
	"/nagi.v1.SettingsService/PromoteStandby": true,
	// 节点信任的面板（地址、指纹）、移交节点、面板的 mesh（面板的地址和所有对端）。
	"/nagi.v1.NodeService/ListNodePanels": true, "/nagi.v1.NodeService/HandoverNodes": true,
	"/nagi.v1.NodeService/RemoveNodePanel": true, "/nagi.v1.NodeService/GetPanelMesh": true,
	// mesh 自动切路总开关：改变所有节点之间的线路，设置页的 mesh 标签也只给管理员。
	"/nagi.v1.SettingsService/SetMeshAutoRoute": true,
	// 面板的更新渠道：决定一键更新面板时会不会装上预发布版。
	"/nagi.v1.SettingsService/SetPanelUpdateChannel": true,
	// 升级全部节点：会升级范围外的节点。
	"/nagi.v1.NodeService/UpgradeAllNodes": true,
	// 重命名、删除分组和标签：会改到成员范围外的项目和节点。
	"/nagi.v1.ProjectService/RenameProjectGroup": true, "/nagi.v1.NodeService/RenameNodeTag": true,
	"/nagi.v1.NodeService/RenameNodeGroup": true, "/nagi.v1.ProjectService/RenameProjectTag": true,
}

// procedurePerms 是其余每个接口要的权限。
var procedurePerms = map[string]Perm{
	// ---- 项目 ----
	"/nagi.v1.ProjectService/ListProjects": PermView, "/nagi.v1.ProjectService/GetProject": PermView,
	"/nagi.v1.ProjectService/ListContainerEvents": PermView, "/nagi.v1.ProjectService/ListProjectEvents": PermView,
	"/nagi.v1.ProjectService/ListVolumes": PermView,
	// 监控：项目页看本项目容器的占用（前缀 c:<项目>/），节点页看节点整体的（另外要节点在范围内，api 的 scope.go）。
	"/nagi.v1.NodeService/QueryNodeMetrics": PermView,

	"/nagi.v1.ProjectService/ListVersions": PermConfig, "/nagi.v1.ProjectService/ListDeployments": PermConfig,

	"/nagi.v1.ProjectService/StreamLogs": PermLogs,

	"/nagi.v1.ProjectService/ControlContainer": PermControl, "/nagi.v1.ProjectService/ControlService": PermControl,
	"/nagi.v1.ProjectService/CheckImages": PermControl,

	"/nagi.v1.ProjectService/AddContainer": PermDeploy, "/nagi.v1.ProjectService/DeleteContainer": PermDeploy,
	"/nagi.v1.ProjectService/SaveDraft": PermDeploy, "/nagi.v1.ProjectService/DiscardDraft": PermDeploy,
	"/nagi.v1.ProjectService/Deploy": PermDeploy, "/nagi.v1.ProjectService/Rollback": PermDeploy,
	"/nagi.v1.ProjectService/RestoreDeployment": PermDeploy, "/nagi.v1.ProjectService/SaveSharedEnv": PermDeploy,
	"/nagi.v1.ProjectService/DiscardSharedEnv": PermDeploy, "/nagi.v1.ProjectService/SetContainerDisplayName": PermDeploy,

	"/nagi.v1.ProjectService/ListVolumeFiles": PermFilesRead, "/nagi.v1.ProjectService/ListVolumeDirSizes": PermFilesRead,

	"/nagi.v1.ProjectService/DeleteVolumeFile": PermFilesWrite, "/nagi.v1.ProjectService/CreateVolumeDir": PermFilesWrite,
	"/nagi.v1.ProjectService/RestoreVolumeFile": PermFilesWrite, "/nagi.v1.ProjectService/RenameVolumeFile": PermFilesWrite,
	"/nagi.v1.ProjectService/MoveVolumeFiles": PermFilesWrite, "/nagi.v1.ProjectService/CopyVolumeFiles": PermFilesWrite,
	"/nagi.v1.ProjectService/CompressVolumeFiles": PermFilesWrite, "/nagi.v1.ProjectService/ExtractVolumeFile": PermFilesWrite,

	"/nagi.v1.ProjectService/ListSnapshots": PermSnapshots, "/nagi.v1.ProjectService/CreateSnapshot": PermSnapshots,
	"/nagi.v1.ProjectService/RestoreSnapshot": PermSnapshots, "/nagi.v1.ProjectService/DeleteSnapshot": PermSnapshots,
	"/nagi.v1.ProjectService/SetContainerSnapshotSchedule": PermSnapshots,

	"/nagi.v1.ProjectService/SetBackupNodes": PermBackup, "/nagi.v1.ProjectService/SyncBackup": PermBackup,
	"/nagi.v1.ProjectService/StopBackupSync": PermBackup, "/nagi.v1.ProjectService/ListBackupSnapshots": PermBackup,
	"/nagi.v1.ProjectService/RestoreFromBackup": PermBackup, "/nagi.v1.ProjectService/GetBackupKey": PermBackup,
	// 节点上的副本属于备份。
	"/nagi.v1.NodeService/ListNodeReplicas": PermBackup,

	"/nagi.v1.ProjectService/SetFailover": PermHA, "/nagi.v1.ProjectService/ClearFailover": PermHA,
	"/nagi.v1.ProjectService/ChangeWitness": PermHA, "/nagi.v1.ProjectService/ConfirmTakeover": PermHA,

	"/nagi.v1.ProjectService/MigrateContainer": PermMigrate, "/nagi.v1.ProjectService/RedeployElsewhere": PermMigrate,

	"/nagi.v1.ProjectService/SaveRoute": PermNetwork, "/nagi.v1.ProjectService/DeleteRoute": PermNetwork,
	"/nagi.v1.ProjectService/CheckDomains": PermNetwork, "/nagi.v1.ProjectService/RetryDomainCert": PermNetwork,
	"/nagi.v1.ProjectService/ConnectProject": PermNetwork, "/nagi.v1.ProjectService/DisconnectProject": PermNetwork,

	"/nagi.v1.ProjectService/SetContainerAlertRules": PermAlerts,

	"/nagi.v1.ProjectService/ListRegistries": PermRegistries, "/nagi.v1.ProjectService/LoginRegistry": PermRegistries,
	"/nagi.v1.ProjectService/LogoutRegistry": PermRegistries,

	// 项目设置里的告警规则、快照、内部端口、出站限制另外要相应的权限（api 的 projects.go）。
	"/nagi.v1.ProjectService/CreateProject": PermProjectManage, "/nagi.v1.ProjectService/DeleteProject": PermProjectManage,
	"/nagi.v1.ProjectService/UpdateProjectSettings": PermProjectManage,
	// 项目标签只用来整理项目页，不影响访问范围，和改项目设置一样要「项目管理」。
	"/nagi.v1.ProjectService/SetProjectTags": PermProjectManage,

	// 分组和标签决定成员的访问范围，归到「成员」。重命名、删除会改到范围外的项目和节点，只给管理员（adminProcedures）。
	"/nagi.v1.ProjectService/SetProjectGroup": PermMembers, "/nagi.v1.NodeService/SetNodeTags": PermMembers,

	// ---- 节点 ----
	"/nagi.v1.NodeService/ListNodes": PermNodeView, "/nagi.v1.NodeService/GetNodeMesh": PermNodeView,
	"/nagi.v1.NodeService/PingNodeMesh": PermNodeView,

	"/nagi.v1.NodeService/RemoveNode": PermNodeManage, "/nagi.v1.NodeService/ApproveNode": PermNodeManage,
	"/nagi.v1.NodeService/RenameNode": PermNodeManage, "/nagi.v1.NodeService/UpgradeNode": PermNodeManage,
	// 节点分组只用来整理节点页，不影响访问范围，和重命名节点一样要「节点管理」。
	"/nagi.v1.NodeService/SetNodeGroup":      PermNodeManage,
	"/nagi.v1.NodeService/CancelNodeUpgrade": PermNodeManage,
	"/nagi.v1.NodeService/GetNodeMirrors":    PermNodeManage, "/nagi.v1.NodeService/SetNodeMirrors": PermNodeManage,
	"/nagi.v1.NodeService/ReclaimNodeSnapshots": PermNodeManage,
	// 项目根目录决定节点以 root 在哪里建目录、写 compose 和数据，也是「容器目录要在根目录内」这条限制的依据：
	// 能改它就等于节点的 root（2026-11-19-87c02b5#4）。要「节点终端和文件」，并先做高权限确认。
	"/nagi.v1.NodeService/SetNodeProjectsRoot": PermNodeRoot,
	"/nagi.v1.NodeService/GetNodeStorage":      PermNodeManage, "/nagi.v1.NodeService/AdoptNodeContainer": PermNodeManage,
	"/nagi.v1.NodeService/AdoptStaleProject": PermNodeManage, "/nagi.v1.NodeService/ConfirmPendingHA": PermNodeManage,
	"/nagi.v1.NodeService/SetNodeAlertRules": PermNodeManage, "/nagi.v1.NodeService/IsolateNode": PermNodeManage,
	"/nagi.v1.NodeService/UnisolateNode": PermNodeManage, "/nagi.v1.NodeService/DeleteNodeReplica": PermNodeManage,
	"/nagi.v1.NodeService/TestNodeMeshSpeed": PermNodeManage, "/nagi.v1.NodeService/SetNodeMeshOptions": PermNodeManage,
	// 中转池：同意这个节点为别的节点承担性能中转（流量计入它），和节点的网络设置一样要「节点管理」。
	"/nagi.v1.NodeService/SetNodeRelayPool": PermNodeManage,
	// 节点的更新渠道：决定节点能不能升级到预发布版，和升级节点一样要「节点管理」。
	"/nagi.v1.NodeService/SetNodeUpdateChannel": PermNodeManage,

	// 网络连接（见 docs/agent.md#网络连接）：连接的对端、进程和容器，和主机名、挂载点一样属于节点详情。命令行另外要「节点终端和文件」（api 的 conns.go）。
	"/nagi.v1.NodeService/WatchNodeConnections": PermNodeDetails,

	"/nagi.v1.NodeService/ListNodeImages": PermNodeImages, "/nagi.v1.NodeService/DeleteNodeImages": PermNodeImages,
	"/nagi.v1.NodeService/PruneNodeImages": PermNodeImages, "/nagi.v1.NodeService/PullNodeImage": PermNodeImages,

	// 节点上的文件、导入现有目录（读主机上任意目录）、副本的存放位置（在节点上任意位置建目录）：等于节点的 root。
	"/nagi.v1.NodeService/ListNodeDir": PermNodeRoot, "/nagi.v1.NodeService/ReadNodeFile": PermNodeRoot,
	"/nagi.v1.NodeService/WriteNodeFile": PermNodeRoot, "/nagi.v1.NodeService/DeleteNodeFile": PermNodeRoot,
	"/nagi.v1.NodeService/CreateNodeDir": PermNodeRoot, "/nagi.v1.NodeService/ListNodeFileBackups": PermNodeRoot,
	"/nagi.v1.NodeService/RestoreNodeFileBackup": PermNodeRoot, "/nagi.v1.NodeService/RenameNodeFile": PermNodeRoot,
	"/nagi.v1.NodeService/MoveNodeFiles": PermNodeRoot, "/nagi.v1.NodeService/CopyNodeFiles": PermNodeRoot,
	"/nagi.v1.NodeService/CompressNodeFiles": PermNodeRoot, "/nagi.v1.NodeService/ExtractNodeFile": PermNodeRoot,
	"/nagi.v1.NodeService/ListNodeDirSizes": PermNodeRoot, "/nagi.v1.NodeService/InspectComposeDir": PermNodeRoot,
	"/nagi.v1.ProjectService/ImportComposeDir": PermNodeRoot, "/nagi.v1.NodeService/SetNodeReplicaDir": PermNodeRoot,
	"/nagi.v1.ProjectService/SetBackupReplicaDir": PermNodeRoot,

	// ---- 面板 ----
	"/nagi.v1.SettingsService/ListUsers": PermMembers, "/nagi.v1.SettingsService/CreateUser": PermMembers,
	"/nagi.v1.SettingsService/UpdateUser": PermMembers, "/nagi.v1.SettingsService/DeleteUser": PermMembers,
	"/nagi.v1.SettingsService/ListRoles": PermMembers, "/nagi.v1.SettingsService/CreateRole": PermMembers,
	"/nagi.v1.SettingsService/UpdateRole": PermMembers, "/nagi.v1.SettingsService/DeleteRole": PermMembers,

	"/nagi.v1.SettingsService/SetPanelDomain": PermSettings, "/nagi.v1.SettingsService/SetPanelName": PermSettings,
	"/nagi.v1.SettingsService/GetAlertConfig": PermSettings, "/nagi.v1.SettingsService/UpdateAlertConfig": PermSettings,
	"/nagi.v1.SettingsService/TestAlertChannel": PermSettings, "/nagi.v1.SettingsService/UpdatePanel": PermSettings,
	"/nagi.v1.SettingsService/GetUpdateStatus": PermSettings,

	"/nagi.v1.SettingsService/GetAuditEvent": PermAudit,
	"/nagi.v1.AlertService/ListAlertStates":  PermAudit,
	"/nagi.v1.AlertService/HandleAlert":      PermAlerts,
	"/nagi.v1.SettingsService/ListActivity":  PermAudit, "/nagi.v1.SettingsService/ExportAuditEvents": PermAudit,
	"/nagi.v1.SettingsService/VerifyAudit": PermAudit, "/nagi.v1.SettingsService/GetRecording": PermAudit,

	// 审计设置（终端录制、保留时间、外送目标、登录安全告警）归「安全设置」：只能看审计的人不能关掉录制、缩短保留时间或改外送去向。
	"/nagi.v1.SettingsService/GetAuditSettings": PermSecurity, "/nagi.v1.SettingsService/SetAuditSettings": PermSecurity,
	"/nagi.v1.SettingsService/TestAuditSink": PermSecurity,
	"/nagi.v1.SettingsService/SetEntrance":   PermSecurity, "/nagi.v1.SettingsService/GetSessionPolicy": PermSecurity,
	"/nagi.v1.SettingsService/SetSessionPolicy": PermSecurity, "/nagi.v1.SettingsService/GetAdminNetworks": PermSecurity,
	"/nagi.v1.SettingsService/SetAdminNetworks": PermSecurity,
}

// ProcedureAccess 说明一个接口怎么登记的（测试和文档用）：public、self、admin 或权限名；没登记时为空。
func ProcedureAccess(procedure string) string {
	switch {
	case publicProcedures[procedure]:
		return "public"
	case selfProcedures[procedure]:
		return "self"
	case adminProcedures[procedure]:
		return "admin"
	}
	if p, ok := procedurePerms[procedure]; ok {
		return p.String()
	}
	return ""
}

// ProcedurePerm 返回接口要的权限。ok 为 false 表示不是按权限登记的（public、self、admin，或者没登记）。
func ProcedurePerm(procedure string) (Perm, bool) {
	p, ok := procedurePerms[procedure]
	return p, ok
}

// IsAdmin 判断是不是管理员。
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// Can 判断成员有没有这项权限。管理员拥有全部权限。访问令牌的选项另由 TokenAllowed 检查。
func (u User) Can(p Perm) bool { return u.IsAdmin() || u.Perms.Has(p) }

// EffectivePerms 是成员实际拥有的权限：管理员是全部。
func (u User) EffectivePerms() Perms {
	if u.IsAdmin() {
		return AllPerms
	}
	return u.Perms
}

// Allowed 判断成员能不能调用接口（procedure 形如 /nagi.v1.ProjectService/Deploy）。没登记的接口一律拒绝。
func Allowed(u User, procedure string) bool {
	switch {
	case publicProcedures[procedure], selfProcedures[procedure]:
		return true
	case adminProcedures[procedure]:
		return u.IsAdmin()
	}
	p, ok := procedurePerms[procedure]
	return ok && u.Can(p)
}

// applyRole 按 users 表里的角色和访问范围填好权限字段。perms 是角色的权限名称，用逗号分隔（store 用 group_concat 读出）。
func (u *User) applyRole(role string, roleID int64, perms string, allProjects, allNodes, follow int64) {
	u.RoleID = roleID
	if role == RoleAdmin {
		u.Role, u.Perms = RoleAdmin, AllPerms
		u.LimitProjects, u.LimitNodes, u.NodesFollow = false, false, false
		return
	}
	u.Perms = ParsePerms(strings.Split(perms, ",")).Normalize()
	u.Role = legacyRole(u.Perms)
	u.LimitProjects = allProjects == 0
	u.NodesFollow = follow != 0
	u.LimitNodes = allNodes == 0 || u.NodesFollow
}

// applyToken 按访问令牌的选项收窄权限（见 docs/mcp.md#权限）：有效权限是角色的权限和令牌选项的交集。
// 选项只在角色有相应权限时生效：「执行命令」要终端权限，「节点终端」要节点终端和文件权限，「管理面板」要面板的任一权限；
// 成员的角色改了，令牌上的选项随之失效。修改类的接口另由 TokenAllowed 按「可修改」检查。
func (u *User) applyToken(write, exec, nodeExec, panel bool) {
	p := u.EffectivePerms()
	u.AllowWrite = write
	u.AllowExec = exec && p.Has(PermTerminal)
	u.AllowNodeExec = nodeExec && p.Has(PermNodeRoot)
	u.AllowPanel = panel && p&PanelPerms != 0
	if !u.AllowExec {
		// 能执行命令本来就能读写容器里的文件；没有这一项的令牌也不能借文件接口改容器里的文件（api 的 checkVolumeWrite）。
		p &^= PermsOf(PermTerminal, PermFilesWrite)
	}
	if !u.AllowNodeExec {
		p &^= PermsOf(PermNodeRoot)
	}
	if !u.AllowPanel {
		p &^= PanelPerms
	}
	// 管理员的 Can 一律放行：令牌上没勾的选项由 TokenAllowed 按接口拦下（节点终端、管理面板的接口都登记在那里）。
	if !u.IsAdmin() {
		u.Perms = p
	}
}

// PanelPerms 是面板的权限。
var PanelPerms = PermsOf(PermMembers, PermSettings, PermAudit, PermSecurity)

// PresetUser 返回预置角色的一个成员（测试和旧版本的角色名用）：admin、operator、viewer，别的没有权限。
func PresetUser(role string) User {
	u := User{ID: 1, RoleID: BuiltinRoleID(role)}
	switch role {
	case RoleAdmin:
		u.applyRole(RoleAdmin, AdminRoleID, "", 1, 1, 0)
	case RoleOperator:
		u.applyRole(RoleMember, OperatorRoleID, strings.Join(PresetOperator.Names(), ","), 1, 1, 0)
	case RoleViewer:
		u.applyRole(RoleMember, ViewerRoleID, strings.Join(PresetViewer.Names(), ","), 1, 1, 0)
	default:
		u.applyRole(RoleMember, 0, "", 1, 1, 0)
	}
	return u
}

// MemberWith 返回有这些权限的成员（测试用）。
func MemberWith(id int64, perms Perms) User {
	u := User{ID: id}
	u.applyRole(RoleMember, 0, strings.Join(perms.Names(), ","), 1, 1, 0)
	return u
}
