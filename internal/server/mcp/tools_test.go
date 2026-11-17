package mcp

import (
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
	readOnly := auth.User{TokenID: 1, AllowExec: true, AllowNodeExec: true}
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
	base := nodeExecCode("n1", "/tmp", "ls", 30*time.Second)
	if base != nodeExecCode("n1", "/tmp", "ls", 30*time.Second) {
		t.Fatal("同样的参数应该得到同样的确认码")
	}
	for i, other := range []string{
		nodeExecCode("n2", "/tmp", "ls", 30*time.Second),
		nodeExecCode("n1", "/", "ls", 30*time.Second),
		nodeExecCode("n1", "/tmp", "ls", 60*time.Second),
		nodeExecCode("n1", "/tmp", "ls; rm -rf /", 30*time.Second),
	} {
		if other == base {
			t.Errorf("case %d: 参数变了，确认码却一样", i)
		}
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
