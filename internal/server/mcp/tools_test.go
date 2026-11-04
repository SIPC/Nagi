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
	readOnly := auth.User{TokenID: 1}
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
