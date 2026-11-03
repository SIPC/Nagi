package mcp

import (
	"strings"
	"testing"

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
			if !auth.Allowed(auth.RoleOperator, p) {
				t.Errorf("工具 %s 用到的接口 %s 操作员不能调用", tl.name, p)
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
