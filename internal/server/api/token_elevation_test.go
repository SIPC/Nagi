package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// tokenElevationRules 是访问令牌对每个要高权限确认的接口的规则（见 docs/mcp.md#高权限确认）：
// "" 表示令牌一律不算确认过；"write" 要勾「可修改」；"node" 要勾「节点终端」；"node+write" 两项都要。都只给管理员。
// elevationRequired 里新加接口时，这里要跟着写明，否则测试失败。
var tokenElevationRules = map[string]string{
	nagiv1connect.AlertServiceHandleAlertProcedure:                 "",
	nagiv1connect.AlertServiceClearAlertsProcedure:                 "",
	nagiv1connect.SettingsServiceTestAuditSinkProcedure:            "",
	nagiv1connect.SettingsServiceCreateAccessTokenProcedure:        "",
	nagiv1connect.SettingsServiceBeginTOTPProcedure:                "",
	nagiv1connect.SettingsServiceBeginPasskeyRegistrationProcedure: "",
	nagiv1connect.SettingsServiceSetAdminNetworksProcedure:         "",
	nagiv1connect.SettingsServiceGetRecordingProcedure:             "",
	nagiv1connect.NodeServiceListNodeDirProcedure:                  "node",
	nagiv1connect.NodeServiceReadNodeFileProcedure:                 "node",
	nagiv1connect.NodeServiceListNodeFileBackupsProcedure:          "node",
	nagiv1connect.NodeServiceListNodeDirSizesProcedure:             "node",
	nagiv1connect.NodeServiceInspectComposeDirProcedure:            "node",
	nagiv1connect.NodeServiceWriteNodeFileProcedure:                "node+write",
	nagiv1connect.NodeServiceDeleteNodeFileProcedure:               "node+write",
	nagiv1connect.NodeServiceCreateNodeDirProcedure:                "node+write",
	nagiv1connect.NodeServiceRestoreNodeFileBackupProcedure:        "node+write",
	nagiv1connect.NodeServiceRenameNodeFileProcedure:               "node+write",
	nagiv1connect.NodeServiceMoveNodeFilesProcedure:                "node+write",
	nagiv1connect.NodeServiceCopyNodeFilesProcedure:                "node+write",
	nagiv1connect.NodeServiceCompressNodeFilesProcedure:            "node+write",
	nagiv1connect.NodeServiceExtractNodeFileProcedure:              "node+write",
	nagiv1connect.ProjectServiceImportComposeDirProcedure:          "node+write",
	nagiv1connect.NodeServiceIsolateNodeProcedure:                  "node+write",
	nagiv1connect.NodeServiceUnisolateNodeProcedure:                "node+write",
	nagiv1connect.NodeServiceConfirmPendingHAProcedure:             "node+write",
	nagiv1connect.NodeServiceSetNodeReplicaDirProcedure:            "node+write",
	nagiv1connect.ProjectServiceSetBackupReplicaDirProcedure:       "node+write",
	nagiv1connect.NodeServiceSetNodeProjectsRootProcedure:          "node+write",
	nagiv1connect.NodeServiceDeleteNodeImagesProcedure:             "write",
	nagiv1connect.NodeServicePruneNodeImagesProcedure:              "write",
	// 添加手机：令牌一律不行（也不在 tokenAccount 里）。
	nagiv1connect.AuthServiceCreateAppPairingProcedure: "",
	// 备用面板、移交节点、成员：令牌一律不行。
	nagiv1connect.SettingsServiceCreateStandbyCodeProcedure: "",
	nagiv1connect.NodeServiceHandoverNodesProcedure:         "",
	nagiv1connect.SettingsServicePromoteStandbyProcedure:    "",
	nagiv1connect.SettingsServiceCreateUserProcedure:        "",
	nagiv1connect.SettingsServiceUpdateUserProcedure:        "",
	// 角色的新建、修改、删除和删除成员：令牌一律不行（都不在 tokenAccount 里）。
	nagiv1connect.SettingsServiceCreateRoleProcedure: "",
	nagiv1connect.SettingsServiceUpdateRoleProcedure: "",
	nagiv1connect.SettingsServiceDeleteRoleProcedure: "",
	nagiv1connect.SettingsServiceDeleteUserProcedure: "",
}

// 每个要确认的接口都按上表判断：各种令牌（勾的选项、角色）能不能调用、算不算确认过；网页的会话只看有没有做过确认。
func TestTokenElevationRules(t *testing.T) {
	for p := range elevationRequired {
		if _, ok := tokenElevationRules[p]; !ok {
			t.Errorf("%s 要高权限确认，但没写访问令牌的规则", p)
		}
	}
	for _, p := range auth.TokenElevatedProcedures() {
		if !elevationRequired[p] {
			t.Errorf("%s 不用高权限确认，不该在 auth.tokenElevated 里", p)
		}
	}
	type token struct {
		name        string
		write, node bool
		role        string
	}
	tokens := []token{
		{"只读", false, false, auth.RoleAdmin}, {"可修改", true, false, auth.RoleAdmin}, {"节点终端", false, true, auth.RoleAdmin},
		{"可修改+节点终端", true, true, auth.RoleAdmin}, {"操作员全勾", true, true, auth.RoleOperator},
	}
	for p, rule := range tokenElevationRules {
		for _, tk := range tokens {
			u := auth.User{ID: 1, Role: tk.role, TokenID: 9, AllowWrite: tk.write, AllowExec: tk.write || tk.node, AllowNodeExec: tk.node}
			want := tk.role == auth.RoleAdmin && map[string]bool{
				"":           false,
				"write":      tk.write,
				"node":       tk.node,
				"node+write": tk.node && tk.write,
			}[rule]
			if got := u.ElevatedFor(p); got != want {
				t.Errorf("%s，%s令牌：算确认过 = %v，想要 %v", p, tk.name, got, want)
			}
			if got := !needsElevation(u, p); got != want {
				t.Errorf("%s，%s令牌：needsElevation 放行 = %v，想要 %v", p, tk.name, got, want)
			}
			// 管理员令牌能不能过 TokenAllowed 和规则一致（设置里的令牌本来就调不了）。
			if tk.role == auth.RoleAdmin && auth.TokenAllowed(u, p) != want {
				t.Errorf("%s，%s令牌：TokenAllowed = %v，想要 %v", p, tk.name, !want, want)
			}
		}
		// 网页的会话：没确认要确认，确认过放行；勾选项对会话不起作用。
		session := auth.User{ID: 1, Role: auth.RoleAdmin, AllowWrite: true, AllowNodeExec: true}
		if !needsElevation(session, p) {
			t.Errorf("%s：没做确认的网页会话应该要确认", p)
		}
		session.ElevatedUntil, session.ElevatedBy = time.Now().Add(time.Minute).Unix(), auth.ElevatedByPassword
		if needsElevation(session, p) {
			t.Errorf("%s：确认过的网页会话应该放行", p)
		}
		// 不是用密码或验证码做的确认（以后 App 用确认密钥做的）：只认密码的接口仍要确认，其余放行。
		session.ElevatedBy = "app-key"
		if got, want := needsElevation(session, p), slices.Contains(auth.PasswordElevationProcedures(), p); got != want {
			t.Errorf("%s：确认密钥做的确认，要再确认 = %v，想要 %v", p, got, want)
		}
	}
	// 不要确认的接口，令牌也不会被这条规则拦下或放行别的。
	if slices.Contains(auth.TokenElevatedProcedures(), nagiv1connect.NodeServiceListNodesProcedure) {
		t.Error("ListNodes 不该在 tokenElevated 里")
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
