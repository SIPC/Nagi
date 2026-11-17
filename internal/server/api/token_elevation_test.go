package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 网页上要先做高权限确认的项目和节点接口，勾了「节点终端」的管理员令牌（MCP）都能用，这样 MCP 能做网页能做的事；
// 设置里要确认的（生成令牌、回放录制、管理员登录地址这些）令牌一律不能算确认过（见 docs/mcp.md#节点文件和节点级操作）。
func TestTokenElevationCoversNodeProcedures(t *testing.T) {
	node := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	for p := range elevationRequired {
		node := node
		project := strings.HasPrefix(p, "/nagi.v1.ProjectService/") || strings.HasPrefix(p, "/nagi.v1.NodeService/")
		if got := node.ElevatedFor(p); got != project {
			t.Errorf("ElevatedFor(%s) = %v，想要 %v", p, got, project)
		}
		if project && !auth.TokenAllowed(node, p) {
			t.Errorf("勾了节点终端的管理员令牌应该能调用 %s", p)
		}
		node.AllowNodeExec = false
		if project && auth.TokenAllowed(node, p) {
			t.Errorf("没勾节点终端的令牌不能调用 %s", p)
		}
	}
}

// 走一遍接口的拦截器：令牌没勾节点终端时角色检查拒绝；勾了的管理员令牌不用高权限确认；网页的会话没确认时要求确认。
func TestElevationCheckTokens(t *testing.T) {
	var user atomic.Value
	_, h := nagiv1connect.NewNodeServiceHandler(nagiv1connect.UnimplementedNodeServiceHandler{},
		connect.WithInterceptors(roleCheck{}, elevationCheck{}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user.Load().(auth.User))))
	}))
	defer srv.Close()
	client := nagiv1connect.NewNodeServiceClient(srv.Client(), srv.URL)
	call := func(u auth.User) (connect.Code, string) {
		user.Store(u)
		_, err := client.ListNodeDir(context.Background(), connect.NewRequest(&nagiv1.NodeFileRef{NodeId: "n1", Path: "/"}))
		var ce *connect.Error
		if !errors.As(err, &ce) {
			return 0, ""
		}
		return ce.Code(), ce.Meta().Get(elevationHeader)
	}
	node := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true}
	if code, _ := call(node); code != connect.CodeUnimplemented {
		t.Errorf("勾了节点终端的管理员令牌应该通过检查，得到 %v", code)
	}
	noNode := node
	noNode.AllowNodeExec = false
	if code, elev := call(noNode); code != connect.CodePermissionDenied || elev != "" {
		t.Errorf("没勾节点终端的令牌应该被角色检查拒绝，得到 %v %q", code, elev)
	}
	if code, elev := call(auth.User{ID: 1, Role: auth.RoleAdmin}); code != connect.CodePermissionDenied || elev != "required" {
		t.Errorf("没做高权限确认的会话应该要求确认，得到 %v %q", code, elev)
	}
	op := node
	op.Role = auth.RoleOperator
	if code, _ := call(op); code != connect.CodePermissionDenied {
		t.Errorf("操作员不能用节点文件，得到 %v", code)
	}
}
