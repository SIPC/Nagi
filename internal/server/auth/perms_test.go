package auth

import (
	"slices"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 浏览器、App 和节点加入时调用的 Connect 接口（api.proto 和 enroll.proto，挂在同一组拦截器上）都要登记到一项权限
// （或者 public、self、admin），没登记的一律拒绝。新增接口忘了登记时这里失败。
func TestEveryProcedureRegistered(t *testing.T) {
	seen := map[string]bool{}
	for _, file := range []protoreflect.FileDescriptor{nagiv1.File_nagi_v1_api_proto, nagiv1.File_nagi_v1_enroll_proto, nagiv1.File_nagi_v1_logs_proto, nagiv1.File_nagi_v1_model_report_proto} {
		services := file.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			if svc.FullName() == "nagi.v1.LogService" || svc.FullName() == "nagi.v1.LogCopyService" {
				// These services authenticate mesh members, never browser users.
				// Even an administrator must not gain an HTTP permission for them.
				for j := range svc.Methods().Len() {
					p := "/nagi.v1." + string(svc.Name()) + "/" + string(svc.Methods().Get(j).Name())
					if Allowed(PresetUser(RoleAdmin), p) || ProcedureAccess(p) != "" {
						t.Errorf("mesh 专用接口 %s 不应登记浏览器权限", p)
					}
				}
				continue
			} // mesh only, not mounted on the panel HTTP API
			for j := range svc.Methods().Len() {
				p := "/nagi.v1." + string(svc.Name()) + "/" + string(svc.Methods().Get(j).Name())
				seen[p] = true
				if ProcedureAccess(p) == "" {
					t.Errorf("%s 没有登记权限（auth/perms.go）", p)
				}
			}
		}
	}
	// 登记的接口都存在，没有写错名字。
	for _, m := range []map[string]bool{publicProcedures, selfProcedures, adminProcedures} {
		for p := range m {
			if !seen[p] {
				t.Errorf("登记的接口 %s 不存在", p)
			}
		}
	}
	for p := range procedurePerms {
		if !seen[p] {
			t.Errorf("登记的接口 %s 不存在", p)
		}
		if publicProcedures[p] || selfProcedures[p] || adminProcedures[p] {
			t.Errorf("%s 登记了两次", p)
		}
	}
	// 没登记的接口谁都不能调用，管理员也不行。
	if Allowed(PresetUser(RoleAdmin), "/nagi.v1.ProjectService/SomethingNew") {
		t.Error("没登记的接口应拒绝")
	}
}

// 每项权限都有名称，名称不重复；每项权限都至少管一个接口或者一个不走 Connect 的入口。
func TestPermNames(t *testing.T) {
	seen := map[string]bool{}
	for p := range permCount {
		n := p.String()
		if n == "" || seen[n] {
			t.Errorf("权限 %d 的名称 %q 为空或重复", p, n)
		}
		seen[n] = true
	}
	used := map[Perm]bool{
		// 不走 Connect 的入口和处理函数里的检查用到的。
		PermTerminal: true, PermFilesRead: true, PermFilesWrite: true, PermAddresses: true, PermPrivileged: true, PermNodeDetails: true,
	}
	for _, p := range procedurePerms {
		used[p] = true
	}
	for p := range permCount {
		if !used[p] {
			t.Errorf("权限 %s 没有管任何接口", p)
		}
	}
	if got := ParsePerms(PermNames()); got != AllPerms {
		t.Errorf("ParsePerms(全部) = %b", got)
	}
	if got := ParsePerms([]string{"project.view", "nope", ""}); got != PermsOf(PermView) {
		t.Errorf("不认识的名称应忽略：%b", got)
	}
}

// 预置角色的权限（收紧后的默认值）。
func TestPresets(t *testing.T) {
	op := []string{"project.view", "project.config", "project.logs", "project.control", "project.deploy", "project.terminal",
		"project.files.read", "project.files.write", "project.snapshots", "node.view"}
	if got := PresetOperator.Names(); !slices.Equal(got, op) {
		t.Errorf("预置操作员：%v", got)
	}
	if got := PresetViewer.Names(); !slices.Equal(got, []string{"project.view", "project.logs", "node.view"}) {
		t.Errorf("预置只读：%v", got)
	}
	// 预置角色都不含「查看访问地址」「查看节点详情」「特权配置」。
	for _, p := range []Perm{PermAddresses, PermNodeDetails, PermPrivileged} {
		if PresetOperator.Has(p) || PresetViewer.Has(p) {
			t.Errorf("预置角色不应含 %s", p)
		}
	}
	if PresetOperator.Normalize() != PresetOperator || PresetViewer.Normalize() != PresetViewer {
		t.Error("预置角色应该已经满足前提")
	}
}

// Normalize 补上前提的权限，去掉只给管理员的。
func TestNormalize(t *testing.T) {
	got := PermsOf(PermDeploy, PermSecurity, PermFilesWrite, PermNodeImages, PermAudit).Normalize()
	want := PermsOf(PermView, PermConfig, PermDeploy, PermFilesRead, PermFilesWrite, PermNodeView, PermNodeImages, PermAudit)
	if got != want {
		t.Errorf("Normalize = %v，想要 %v", got.Names(), want.Names())
	}
}

// 「审计日志」可以授予（不再只给管理员）：有它能调用审计的接口，没有时被拒绝；审计设置归「安全设置」，仍只给管理员。
func TestAuditPermGrantable(t *testing.T) {
	if AdminOnlyPerms.Has(PermAudit) || !AdminOnlyPerms.Has(PermSecurity) {
		t.Fatal("审计日志可以授予，安全设置只给管理员")
	}
	if PresetOperator.Has(PermAudit) || PresetViewer.Has(PermAudit) {
		t.Error("预置的操作员、只读默认没有审计日志")
	}
	auditor := MemberWith(5, PermsOf(PermAudit))
	if !auditor.Perms.Has(PermAudit) {
		t.Fatal("角色里的审计日志被去掉了")
	}
	others := MemberWith(5, AllPerms&^PermsOf(PermAudit))
	for _, p := range []string{"/nagi.v1.SettingsService/GetAuditEvent", "/nagi.v1.SettingsService/ExportAuditEvents",
		"/nagi.v1.SettingsService/VerifyAudit", "/nagi.v1.SettingsService/ListActivity", "/nagi.v1.SettingsService/GetRecording"} {
		if !Allowed(auditor, p) {
			t.Errorf("有审计日志应能调用 %s", p)
		}
		if Allowed(others, p) {
			t.Errorf("没有审计日志不应能调用 %s", p)
		}
	}
	for _, p := range []string{"/nagi.v1.SettingsService/GetAuditSettings", "/nagi.v1.SettingsService/SetAuditSettings", "/nagi.v1.SettingsService/TestAuditSink"} {
		if Allowed(auditor, p) || Allowed(others, p) || !Allowed(PresetUser(RoleAdmin), p) {
			t.Errorf("%s 只给管理员", p)
		}
	}
}

// 「只有查看和日志」的角色：项目能看、日志能看，别的都不行（高可用、备份等标签对应的接口都被拒绝）。
func TestViewLogsRole(t *testing.T) {
	u := MemberWith(5, PermsOf(PermView, PermLogs))
	for _, p := range []string{"/nagi.v1.ProjectService/GetProject", "/nagi.v1.ProjectService/StreamLogs", "/nagi.v1.ProjectService/ListProjects"} {
		if !Allowed(u, p) {
			t.Errorf("应能调用 %s", p)
		}
	}
	for _, p := range []string{"/nagi.v1.ProjectService/SetFailover", "/nagi.v1.ProjectService/SetBackupNodes", "/nagi.v1.ProjectService/ListBackupSnapshots",
		"/nagi.v1.ProjectService/ListVersions", "/nagi.v1.ProjectService/Deploy", "/nagi.v1.ProjectService/CheckDomains",
		"/nagi.v1.ProjectService/ListSnapshots", "/nagi.v1.NodeService/ListNodes", "/nagi.v1.SettingsService/ListUsers",
		"/nagi.v1.NodeService/ListNodePanels", "/nagi.v1.ProjectService/ListRegistries"} {
		if Allowed(u, p) {
			t.Errorf("不应能调用 %s", p)
		}
	}
	// 每项权限给和不给：接口按登记的权限放行。
	for p, need := range procedurePerms {
		with := MemberWith(5, PermsOf(need))
		without := User{ID: 5, Role: RoleOperator, Perms: AllPerms &^ PermsOf(need)} // 不补前提：只缺这一项
		if !Allowed(with, p) && !AdminOnlyPerms.Has(need) {
			t.Errorf("有 %s 应能调用 %s", need, p)
		}
		if Allowed(without, p) {
			t.Errorf("没有 %s 不应能调用 %s", need, p)
		}
	}
	for p := range adminProcedures {
		if Allowed(MemberWith(5, AllPerms), p) {
			t.Errorf("只给管理员的 %s 不应放行别的成员", p)
		}
	}
}

// 访问令牌的有效权限是角色的权限和令牌选项的交集。
func TestTokenIntersection(t *testing.T) {
	op := PresetUser(RoleOperator)
	op.TokenID = 1
	op.applyToken(true, true, true, true)
	if !op.AllowExec || op.AllowNodeExec || op.AllowPanel {
		t.Errorf("操作员的令牌：执行命令 %v、节点终端 %v、管理面板 %v", op.AllowExec, op.AllowNodeExec, op.AllowPanel)
	}
	viewer := PresetUser(RoleViewer)
	viewer.TokenID = 1
	viewer.applyToken(true, true, false, false)
	if viewer.AllowExec || viewer.Can(PermTerminal) {
		t.Error("只读角色没有终端，令牌上的执行命令不生效")
	}
	if !TokenAllowed(viewer, "/nagi.v1.ProjectService/Deploy") || Allowed(viewer, "/nagi.v1.ProjectService/Deploy") {
		t.Error("令牌勾了可修改，角色没有部署：接口仍然拒绝（两边都要满足）")
	}
	noExec := PresetUser(RoleOperator)
	noExec.TokenID = 1
	noExec.applyToken(true, false, false, false)
	if noExec.Can(PermTerminal) || noExec.Can(PermFilesWrite) || !noExec.Can(PermDeploy) {
		t.Error("没勾执行命令的令牌：没有终端和文件修改，其余照旧")
	}
	readOnly := PresetUser(RoleOperator)
	readOnly.TokenID = 1
	readOnly.applyToken(false, false, false, false)
	if TokenAllowed(readOnly, "/nagi.v1.ProjectService/Deploy") || !TokenAllowed(readOnly, "/nagi.v1.ProjectService/GetProject") {
		t.Error("没勾可修改的令牌只能查询")
	}
	// 有「节点终端和文件」的自定义角色可以勾节点终端。
	root := MemberWith(3, PermsOf(PermNodeRoot, PermTerminal))
	root.TokenID = 1
	root.applyToken(true, true, true, false)
	if !root.AllowNodeExec || !root.ElevatedFor("/nagi.v1.NodeService/ReadNodeFile") {
		t.Error("有节点终端和文件的成员，令牌勾了节点终端")
	}
}

// 给旧版本客户端看的角色名。
func TestLegacyRole(t *testing.T) {
	cases := map[string]Perms{RoleViewer: PermsOf(PermView, PermLogs, PermConfig), RoleOperator: PermsOf(PermView, PermSnapshots)}
	for want, p := range cases {
		if got := LegacyRole(false, p); got != want {
			t.Errorf("%v → %s，想要 %s", p.Names(), got, want)
		}
	}
	if LegacyRole(true, 0) != RoleAdmin {
		t.Error("管理员")
	}
	if !strings.HasPrefix(PresetUser(RoleOperator).Role, "operator") {
		t.Error(PresetUser(RoleOperator).Role)
	}
}
