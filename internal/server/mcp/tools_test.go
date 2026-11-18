package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 每个工具声明的权限要和它用到的接口一致：查看类的工具只能用查询接口，修改类的至少用到一个修改接口；
// 用到的接口都存在，操作员能调用（只读成员调不了修改接口，由令牌权限挡住）。新增工具时这里会检查。
func TestToolDeclarations(t *testing.T) {
	file := nagiv1.File_nagi_v1_api_proto
	exists := func(procedure string) bool {
		parts := strings.Split(strings.TrimPrefix(procedure, "/nagi.v1."), "/")
		svc := file.Services().ByName(protoreflect.Name(parts[0]))
		return svc != nil && svc.Methods().ByName(protoreflect.Name(parts[1])) != nil
	}
	// 数修改接口用的令牌：除了「可修改」都勾上（节点文件这类接口还要「节点终端」）。
	readOnly := auth.User{TokenID: 1, AllowExec: true, AllowNodeExec: true, AllowPanel: true}
	seen := map[string]bool{}
	for _, tl := range allTools {
		if seen[tl.name] {
			t.Errorf("工具 %s 重复", tl.name)
		}
		seen[tl.name] = true
		if tl.add == nil || tl.description == "" || len(tl.procedures) == 0 {
			t.Errorf("工具 %s 没有声明完整", tl.name)
		}
		writes := 0
		for _, p := range tl.procedures {
			if !exists(p) {
				t.Errorf("工具 %s 用到的接口 %s 不存在", tl.name, p)
			}
			role := auth.RoleOperator
			if tl.adminOnly {
				role = auth.RoleAdmin
			}
			if !auth.Allowed(role, p) {
				t.Errorf("工具 %s 用到的接口 %s %s 不能调用", tl.name, p, role)
			}
			// 管理员专用的接口，工具也要声明为管理员专用。
			if !tl.adminOnly && !auth.Allowed(auth.RoleOperator, p) {
				t.Errorf("工具 %s 用到只有管理员能调用的接口 %s，要声明 adminOnly", tl.name, p)
			}
			if !auth.TokenAllowed(readOnly, p) {
				writes++
			}
		}
		switch {
		case !tl.write && writes > 0:
			t.Errorf("工具 %s 声明为查看，却用到修改接口", tl.name)
		case tl.write && writes == 0:
			t.Errorf("工具 %s 声明为修改，却只用到查询接口", tl.name)
		}
	}
}

func TestTokenAllowed(t *testing.T) {
	read := auth.User{TokenID: 1}
	write := auth.User{TokenID: 1, AllowWrite: true}
	cases := []struct {
		u         auth.User
		procedure string
		want      bool
	}{
		{read, "/nagi.v1.ProjectService/GetProject", true},
		{read, "/nagi.v1.ProjectService/Deploy", false},
		{write, "/nagi.v1.ProjectService/Deploy", true},
		// 令牌只能用项目和节点的接口：不能管成员、设置、令牌自己。
		{write, "/nagi.v1.SettingsService/ListUsers", false},
		{write, "/nagi.v1.SettingsService/CreateAccessToken", false},
		{write, "/nagi.v1.AuthService/ChangePassword", false},
		// 节点文件这类接口要「节点终端」：只勾了可修改、执行命令不够；读不用「可修改」，写要。
		{auth.User{TokenID: 1, AllowWrite: true, AllowExec: true}, "/nagi.v1.NodeService/ReadNodeFile", false},
		{auth.User{TokenID: 1, AllowExec: true, AllowNodeExec: true}, "/nagi.v1.NodeService/ReadNodeFile", true},
		{auth.User{TokenID: 1, AllowExec: true, AllowNodeExec: true}, "/nagi.v1.NodeService/WriteNodeFile", false},
		{auth.User{TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}, "/nagi.v1.NodeService/WriteNodeFile", true},
		{write, "/nagi.v1.ProjectService/ImportComposeDir", false},
		{write, "/nagi.v1.NodeService/IsolateNode", false},
		{write, "/nagi.v1.NodeService/RemoveNode", true},
	}
	for _, c := range cases {
		if got := auth.TokenAllowed(c.u, c.procedure); got != c.want {
			t.Errorf("TokenAllowed(%+v, %s) = %v，想要 %v", c.u, c.procedure, got, c.want)
		}
	}
}

func TestImageName(t *testing.T) {
	for image, want := range map[string]string{
		"nginx": "nginx",
		"ghcr.io/umami-software/umami:postgresql-latest": "umami",
		"library/Redis@sha256:abc":                       "redis",
		"registry:5000/team/my_app:1":                    "my-app",
	} {
		if got := imageName(image); got != want {
			t.Errorf("imageName(%q) = %q，想要 %q", image, got, want)
		}
	}
}

func TestCalledTools(t *testing.T) {
	one := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{}}}`
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"deploy"}}]`
	if got := calledTools([]byte(one)); len(got) != 1 || got[0] != "exec" {
		t.Errorf("单个请求：%v", got)
	}
	if got := calledTools([]byte(batch)); len(got) != 1 || got[0] != "deploy" {
		t.Errorf("批量请求：%v", got)
	}
	if got := calledTools([]byte("not json")); len(got) != 0 {
		t.Errorf("不是 JSON：%v", got)
	}
}

// 只读令牌、非管理员看不到的工具，各自的判断要对。
func TestAllowedFor(t *testing.T) {
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	op := auth.User{Role: auth.RoleOperator, TokenID: 1, AllowWrite: true}
	admin := auth.User{Role: auth.RoleAdmin, TokenID: 1}
	cases := []struct {
		tool    string
		u       auth.User
		standby bool
		want    bool
	}{
		{"deploy", op, false, true},
		{"deploy", op, true, false},
		{"exec", op, false, false},
		{"restore_snapshot", auth.User{Role: auth.RoleOperator, TokenID: 1}, false, false},
		{"inspect_node_storage", op, false, false},
		{"inspect_node_storage", admin, false, true},
		{"list_snapshots", auth.User{Role: auth.RoleViewer, TokenID: 1}, false, true},
	}
	for _, c := range cases {
		if got := byName[c.tool].allowedFor(c.u, c.standby); got != c.want {
			t.Errorf("%s 对 %+v（备用面板 %v）= %v，想要 %v", c.tool, c.u, c.standby, got, c.want)
		}
	}
}

// node_exec 等于节点的 root：只给管理员、令牌勾了节点终端，备用面板上也没有。
func TestNodeExecAllowed(t *testing.T) {
	var ne tool
	for _, tl := range allTools {
		if tl.name == "node_exec" {
			ne = tl
		}
	}
	admin := auth.User{TokenID: 1, Role: auth.RoleAdmin, AllowWrite: true, AllowExec: true}
	full := admin
	full.AllowNodeExec = true
	operator := full
	operator.Role = auth.RoleOperator
	cases := []struct {
		u       auth.User
		standby bool
		want    bool
	}{
		{admin, false, false},
		{full, false, true},
		{full, true, false},
		{operator, false, false},
	}
	for i, c := range cases {
		if got := ne.allowedFor(c.u, c.standby); got != c.want {
			t.Errorf("case %d: allowedFor = %v，想要 %v", i, got, c.want)
		}
	}
}

// 节点命令的确认码绑定节点、工作目录、超时和命令：预览之后改了哪一项都不能用原来的确认码执行。
func TestNodeExecCode(t *testing.T) {
	c := &call{u: auth.User{ID: 1, TokenID: 7}}
	code := func(nodeID, cwd, command string, timeout time.Duration) string {
		return c.confirmCode(nodeExecParts(nodeID, cwd, command, timeout)...)
	}
	base := code("n1", "/tmp", "ls", 30*time.Second)
	if base != code("n1", "/tmp", "ls", 30*time.Second) {
		t.Fatal("同样的参数应该得到同样的确认码")
	}
	for i, other := range []string{
		code("n2", "/tmp", "ls", 30*time.Second),
		code("n1", "/", "ls", 30*time.Second),
		code("n1", "/tmp", "ls", 60*time.Second),
		code("n1", "/tmp", "ls; rm -rf /", 30*time.Second),
	} {
		if other == base {
			t.Errorf("case %d: 参数变了，确认码却一样", i)
		}
	}
}

// 确认码只能来自预览（见 docs/mcp.md#确认码）：调用方按参数自己算不出来（混入了面板进程的随机密钥），
// 别的令牌预览得到的用不了，过了有效期作废。
func TestConfirmCodeOnlyFromPreview(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	confirmNow = func() time.Time { return now }
	defer func() { confirmNow = time.Now }()
	a := &call{u: auth.User{ID: 1, TokenID: 7}}
	parts := []string{"delete", "n1", "sha256:abc||"}
	code := a.confirmCode(parts...)
	// 旧的算法：不带密钥的 sha256(参数)。
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	if code == hex.EncodeToString(sum[:6]) {
		t.Fatal("确认码不能只由参数算出")
	}
	if _, ok := a.confirm(code, parts...); !ok {
		t.Fatal("刚预览得到的确认码应该能用")
	}
	if _, ok := a.confirm(code, "delete", "n1", "sha256:abd||"); ok {
		t.Fatal("参数变了不能用")
	}
	if _, ok := (&call{u: auth.User{ID: 1, TokenID: 8}}).confirm(code, parts...); ok {
		t.Fatal("别的令牌不能用")
	}
	if _, ok := (&call{u: auth.User{ID: 2, TokenID: 7}}).confirm(code, parts...); ok {
		t.Fatal("别的成员不能用")
	}
	if _, ok := a.confirm("", parts...); ok {
		t.Fatal("空的不能用")
	}
	// 至少 10 分钟内有效，至多 20 分钟。
	now = now.Add(confirmWindow + confirmWindow/2)
	if _, ok := a.confirm(code, parts...); !ok {
		t.Fatal("15 分钟以内（上一个时间段）应该还能用")
	}
	now = now.Add(confirmWindow)
	if _, ok := a.confirm(code, parts...); ok {
		t.Fatal("过期的确认码不能用")
	}
}

// 令牌做不了高权限确认：只有勾了「节点终端」的管理员令牌调用节点文件这类接口时算确认过；别的要确认的接口一律不算。
func TestElevatedFor(t *testing.T) {
	node := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	noNode := node
	noNode.AllowNodeExec = false
	operator := node
	operator.Role = auth.RoleOperator
	session := auth.User{ID: 1, Role: auth.RoleAdmin}
	elevated := session
	elevated.ElevatedUntil = time.Now().Add(time.Minute).Unix()
	cases := []struct {
		u         auth.User
		procedure string
		want      bool
	}{
		{node, "/nagi.v1.NodeService/ListNodeDir", true},
		{node, "/nagi.v1.ProjectService/ImportComposeDir", true},
		{node, "/nagi.v1.NodeService/IsolateNode", true},
		{noNode, "/nagi.v1.NodeService/ListNodeDir", false},
		{operator, "/nagi.v1.NodeService/ListNodeDir", false},
		{node, "/nagi.v1.SettingsService/CreateAccessToken", false},
		{node, "/nagi.v1.SettingsService/GetRecording", false},
		{node, "/nagi.v1.SettingsService/SetAdminNetworks", false},
		{session, "/nagi.v1.NodeService/ListNodeDir", false},
		{elevated, "/nagi.v1.NodeService/ListNodeDir", true},
	}
	for i, c := range cases {
		if got := c.u.ElevatedFor(c.procedure); got != c.want {
			t.Errorf("case %d: ElevatedFor(%s) = %v，想要 %v", i, c.procedure, got, c.want)
		}
	}
}

// 新加的工具按权限分给令牌：节点文件和节点级操作只给勾了「节点终端」的管理员令牌；节点管理只给管理员；
// 卷文件的移动这类要执行命令和可修改；备用面板上都没有。
func TestNewToolsAllowedFor(t *testing.T) {
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	nodeAdmin := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	nodeAdminRead := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowExec: true, AllowNodeExec: true}
	admin := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true}
	adminRead := auth.User{Role: auth.RoleAdmin, TokenID: 1}
	op := auth.User{Role: auth.RoleOperator, TokenID: 1, AllowWrite: true, AllowExec: true}
	opNoExec := auth.User{Role: auth.RoleOperator, TokenID: 1, AllowWrite: true}
	viewer := auth.User{Role: auth.RoleViewer, TokenID: 1}
	cases := []struct {
		tool    string
		u       auth.User
		standby bool
		want    bool
	}{
		{"list_node_files", nodeAdminRead, false, true},
		{"list_node_files", admin, false, false},
		{"list_node_files", nodeAdmin, true, false},
		{"read_node_file", nodeAdminRead, false, true},
		{"write_node_file", nodeAdminRead, false, false},
		{"write_node_file", nodeAdmin, false, true},
		{"delete_node_file", admin, false, false},
		{"import_compose_dir", admin, false, false},
		{"import_compose_dir", nodeAdmin, false, true},
		{"isolate_node", admin, false, false},
		{"isolate_node", nodeAdmin, false, true},
		{"confirm_pending_ha", admin, false, false},
		{"remove_node", admin, false, true},
		{"remove_node", op, false, false},
		{"remove_node", adminRead, false, false},
		{"upgrade_nodes", admin, true, false},
		{"get_node_mirrors", adminRead, false, true},
		{"get_node_mirrors", op, false, false},
		{"get_node", viewer, false, true},
		{"check_mesh", viewer, false, true},
		{"list_versions", viewer, false, true},
		{"set_alert_rules", op, false, true},
		{"set_alert_rules", viewer, false, false},
		{"set_node_alert_rules", op, false, false},
		{"move_files", op, false, true},
		{"move_files", opNoExec, false, false},
		{"move_files", viewer, false, false},
		{"compress_files", op, true, false},
		{"rename_project_group", op, false, false},
		{"rename_project_group", admin, false, true},
	}
	for _, c := range cases {
		tl, ok := byName[c.tool]
		if !ok {
			t.Fatalf("没有工具 %s", c.tool)
		}
		if got := tl.allowedFor(c.u, c.standby); got != c.want {
			t.Errorf("%s 对 %+v（备用面板 %v）= %v，想要 %v", c.tool, c.u, c.standby, got, c.want)
		}
	}
	// 节点级的工具都要声明 nodeExec，修改节点文件的还要声明 destructive（客户端会先问用户）。
	for _, tl := range allTools {
		for _, p := range tl.procedures {
			if !auth.TokenAllowed(auth.User{TokenID: 1, AllowWrite: true, AllowExec: true}, p) && auth.TokenAllowed(nodeAdmin, p) && !tl.nodeExec {
				t.Errorf("工具 %s 用到要「节点终端」的接口 %s，要声明 nodeExec", tl.name, p)
			}
		}
	}
}

// 设置和账号的接口：令牌只能碰管理面板列出的这些（还要勾「管理面板」）和自己的项目页排序，其余一律不能，
// 勾满所有选项的管理员令牌也不能。新加的设置、账号接口默认令牌不能用；要开放时在这里和 auth.tokenAccount 里写明。
func TestTokenNeverReaches(t *testing.T) {
	panelRead := []string{"GetSettings", "GetAlertConfig", "ListActivity", "ListAuditEvents", "GetAuditEvent", "ExportAuditEvents", "VerifyAudit",
		"CheckPanelUpdate", "GetPanelReleaseNotes", "GetUpdateStatus", "GetStandbys", "ListUsers"}
	panelWrite := []string{"UpdateSettings", "SetPanelDomain", "UpdateAlertConfig", "TestAlertChannel", "UpdatePanel"}
	never := []string{"CreateAccessToken", "RevokeAccessToken", "ListAccessTokens", "BeginTOTP", "EnableTOTP", "DisableTOTP", "GetSecurity",
		"BeginPasskeyRegistration", "AddPasskey", "DeletePasskey", "ListSessions", "RevokeSession", "SetEntrance", "CreateStandbyCode",
		"RemoveStandby", "PromoteStandby", "CreateInvite", "ListInvites", "RevokeInvite", "CreateUser", "UpdateUser", "DeleteUser",
		"GetSessionPolicy", "SetSessionPolicy", "GetAuditSettings", "SetAuditSettings", "TestAuditSink", "GetAdminNetworks", "SetAdminNetworks",
		"GetRecording", "Login", "Logout", "Me", "Elevate", "ChangePassword", "SetLanguage", "UpdateProfile", "GetOAuthRequest", "ApproveOAuth",
		"RegisterSessionKey", "RotateSession", "Reverify", "BeginPasskeyLogin", "PasskeyLogin", "GetSetupState", "Setup"}
	full := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true, AllowPanel: true}
	noPanel := full
	noPanel.AllowPanel = false
	readPanel := full
	readPanel.AllowWrite = false
	file := nagiv1.File_nagi_v1_api_proto
	for _, svc := range []string{"SettingsService", "AuthService", "SetupService"} {
		methods := file.Services().ByName(protoreflect.Name(svc)).Methods()
		for i := range methods.Len() {
			m := string(methods.Get(i).Name())
			p := "/nagi.v1." + svc + "/" + m
			want := slices.Contains(panelRead, m) || slices.Contains(panelWrite, m) || m == "GetProjectOrder" || m == "SetProjectOrder"
			if got := auth.TokenAllowed(full, p); got != want {
				t.Errorf("勾满选项的管理员令牌调用 %s = %v，想要 %v", p, got, want)
			}
			if slices.Contains(never, m) && auth.TokenAllowed(full, p) {
				t.Errorf("令牌永远不能调用 %s", p)
			}
			if wantNo := m == "GetProjectOrder" || m == "SetProjectOrder"; auth.TokenAllowed(noPanel, p) != wantNo {
				t.Errorf("没勾管理面板的令牌调用 %s = %v，想要 %v", p, !wantNo, wantNo)
			}
			if wantRead := slices.Contains(panelRead, m) || m == "GetProjectOrder"; auth.TokenAllowed(readPanel, p) != wantRead {
				t.Errorf("只读的管理面板令牌调用 %s = %v，想要 %v", p, !wantRead, wantRead)
			}
		}
	}
	// 只读令牌（什么都没勾）只能看自己的项目页排序。
	if !auth.TokenAllowed(auth.User{TokenID: 1}, "/nagi.v1.AuthService/GetProjectOrder") || auth.TokenAllowed(auth.User{TokenID: 1}, "/nagi.v1.AuthService/SetProjectOrder") {
		t.Error("只读令牌：能看项目页排序，不能改")
	}
}

// 管理面板的工具只给勾了「管理面板」的管理员令牌；修改的还要「可修改」，备用面板上没有。成员被降为操作员后，
// 令牌上的这一项随之失效（auth.TokenUser 只给管理员 AllowPanel，这里看角色）。
func TestPanelToolsAllowedFor(t *testing.T) {
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	full := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowPanel: true}
	read := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowPanel: true}
	noPanel := auth.User{Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	op := auth.User{Role: auth.RoleOperator, TokenID: 1, AllowWrite: true, AllowPanel: true}
	cases := []struct {
		tool    string
		u       auth.User
		standby bool
		want    bool
	}{
		{"get_panel_settings", read, false, true},
		{"get_panel_settings", noPanel, false, false},
		{"get_panel_settings", op, false, false},
		{"query_audit", read, true, true},
		{"update_panel_settings", read, false, false},
		{"update_panel_settings", full, false, true},
		{"update_panel_settings", full, true, false},
		{"update_alert_channels", full, false, true},
		{"update_alert_channels", noPanel, false, false},
		{"update_panel", full, false, true},
		{"list_members", read, false, true},
		{"get_project_order", auth.User{Role: auth.RoleViewer, TokenID: 1}, false, true},
		{"set_project_order", auth.User{Role: auth.RoleViewer, TokenID: 1}, false, false},
		{"set_project_order", auth.User{Role: auth.RoleOperator, TokenID: 1, AllowWrite: true}, false, true},
	}
	for _, c := range cases {
		tl, ok := byName[c.tool]
		if !ok {
			t.Fatalf("没有工具 %s", c.tool)
		}
		if got := tl.allowedFor(c.u, c.standby); got != c.want {
			t.Errorf("%s 对 %+v（备用面板 %v）= %v，想要 %v", c.tool, c.u, c.standby, got, c.want)
		}
	}
	// 用到设置接口的工具都要声明 panel（每次调用发告警），声明了 panel 的 OAuth 范围是 nagi:admin。
	for _, tl := range allTools {
		uses := slices.ContainsFunc(tl.procedures, func(p string) bool { return strings.HasPrefix(p, "/nagi.v1.SettingsService/") })
		if uses != tl.panel {
			t.Errorf("工具 %s：用到设置接口 %v，声明 panel %v", tl.name, uses, tl.panel)
		}
		if tl.panel && (tl.scope() != "nagi:admin" || !tl.adminOnly) {
			t.Errorf("工具 %s 要只给管理员，OAuth 范围是 nagi:admin", tl.name)
		}
	}
}

func regexpAll(expr, text string) []string {
	return regexp.MustCompile(expr).FindAllString(text, -1)
}

// 服务说明里的部署规则：提到的资源都存在，提到的工具参数和实际的一致。
func TestInstructionsDeploymentRules(t *testing.T) {
	text := instructions(auth.User{Name: "ink", Role: auth.RoleAdmin, AllowWrite: true}, false)
	for _, want := range []string{"Deployment rules", "image form", "env_secret", "volumes", "host_ip", "save_route", "connect_project", "node_exec", "warnings"} {
		if !strings.Contains(text, want) {
			t.Errorf("服务说明里应该有 %q", want)
		}
	}
	uris := map[string]bool{}
	for _, d := range docs {
		uris[d.uri] = true
	}
	for _, u := range regexpAll(`nagi://[a-z/]+[a-z]`, text) {
		if !uris[u] {
			t.Errorf("服务说明提到的资源 %s 不存在", u)
		}
	}
	for _, d := range docs {
		for _, u := range regexpAll(`nagi://[a-z/]+[a-z]`, d.text) {
			if !uris[u] {
				t.Errorf("资源 %s 提到的资源 %s 不存在", d.uri, u)
			}
		}
	}
	if !uris["nagi://docs/deploy"] {
		t.Fatal("缺少部署指南资源")
	}
	// 说明里、指南里写的参数名要和工具的参数一致。
	params := func(v any) map[string]bool {
		out := map[string]bool{}
		var walk func(reflect.Type)
		walk = func(t reflect.Type) {
			for i := range t.NumField() {
				f := t.Field(i)
				if f.Anonymous {
					walk(f.Type)
					continue
				}
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				out[name] = true
			}
		}
		walk(reflect.TypeOf(v))
		return out
	}
	add, update, deploy := params(AddContainerArgs{}), params(UpdateContainerArgs{}), params(DeployArgs{})
	for _, p := range []string{"image", "compose", "env", "env_secret", "service_env", "volumes", "health_command", "depends_on", "stateful", "preserved_paths", "workdir", "memory_mb", "git_url"} {
		if !add[p] {
			t.Errorf("add_container 没有参数 %s", p)
		}
	}
	for _, p := range []string{"env", "env_secret", "service_env", "volumes", "ports", "expected_revision"} {
		if !update[p] {
			t.Errorf("update_container 没有参数 %s", p)
		}
	}
	for _, p := range []string{"apply_shared_env", "pull_git", "wait_healthy_seconds", "expected_revisions"} {
		if !deploy[p] {
			t.Errorf("deploy 没有参数 %s", p)
		}
	}
	if !params(UpdateProjectArgs{})["internal_ports"] {
		t.Error("update_project 没有参数 internal_ports")
	}
	names := map[string]bool{}
	for _, tl := range allTools {
		names[tl.name] = true
	}
	for _, d := range docs {
		for _, n := range regexpAll(`\b[a-z]+(_[a-z]+)+\b`, d.text) {
			if strings.HasSuffix(n, "_container") || strings.HasPrefix(n, "save_") || strings.HasPrefix(n, "update_") || strings.HasPrefix(n, "check_") ||
				strings.HasSuffix(n, "_project") || strings.HasSuffix(n, "_registry") || strings.HasSuffix(n, "_cert") || strings.HasSuffix(n, "_exec") {
				if !names[n] {
					t.Errorf("资源 %s 提到的工具 %s 不存在", d.uri, n)
				}
			}
		}
	}
}
