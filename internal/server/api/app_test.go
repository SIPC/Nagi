package api

import (
	"testing"

	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 只认密码或验证码的高权限确认（见 docs/android.md#高权限确认和重新验证）：每一项都要先确认。
func TestPasswordElevationListed(t *testing.T) {
	for _, p := range auth.PasswordElevationProcedures() {
		if !elevationRequired[p] {
			t.Errorf("%s 只认密码确认，但不在 elevationRequired 里", p)
		}
	}
	// 添加手机：App 的会话（没有 ElevatedBy）即使确认过也不行。
	app := auth.User{ID: 1, Role: auth.RoleAdmin, DeviceID: "d", ElevatedUntil: 1 << 40}
	if !needsElevation(app, nagiv1connect.AuthServiceCreateAppPairingProcedure) {
		t.Error("App 不能生成配对码")
	}
}

// App 的接口所有角色都能调用（换取令牌另有规则）；备用面板上放行换取令牌和查询。
func TestAppProcedureRoles(t *testing.T) {
	for _, p := range []string{nagiv1connect.AppServiceInfoProcedure, nagiv1connect.AppServiceLogoutProcedure} {
		for _, role := range []string{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
			if !auth.Allowed(auth.PresetUser(role), p) {
				t.Errorf("%s 不能调用 %s", role, p)
			}
		}
	}
	for _, p := range []string{nagiv1connect.AppServiceTokenProcedure,
		nagiv1connect.AppServiceChallengeProcedure, nagiv1connect.AppServiceInfoProcedure} {
		if !readOnlyProcedure(p) {
			t.Errorf("备用面板上应放行 %s", p)
		}
	}
	for _, p := range []string{nagiv1connect.AppServicePairProcedure, nagiv1connect.AuthServiceCreateAppPairingProcedure} {
		if readOnlyProcedure(p) {
			t.Errorf("备用面板上不该放行 %s", p)
		}
	}
	// 访问令牌（MCP）调不了 App 的接口和生成配对码。
	tok := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 2, AllowWrite: true, AllowPanel: true, AllowNodeExec: true}
	for _, p := range []string{nagiv1connect.AppServiceInfoProcedure, nagiv1connect.AuthServiceCreateAppPairingProcedure} {
		if auth.TokenAllowed(tok, p) {
			t.Errorf("访问令牌能调用 %s", p)
		}
	}
}
