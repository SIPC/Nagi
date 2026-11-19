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

// 备用面板的配对码、移交节点、提升备用面板、新建和修改成员只认密码或验证码做的确认，手机上的确认不算。
// 访问令牌一律不行；备用面板上能做高权限确认（提升前要做）。
func TestPanelTakeoverNeedsElevation(t *testing.T) {
	passwordOnly := []string{nagiv1connect.SettingsServiceCreateStandbyCodeProcedure,
		nagiv1connect.NodeServiceHandoverNodesProcedure, nagiv1connect.SettingsServicePromoteStandbyProcedure,
		nagiv1connect.SettingsServiceCreateUserProcedure, nagiv1connect.SettingsServiceUpdateUserProcedure}
	plain := auth.User{ID: 1, Role: auth.RoleAdmin}
	app := auth.User{ID: 1, Role: auth.RoleAdmin, DeviceID: "d", ElevatedUntil: 1 << 40, ElevatedBy: auth.ElevatedByApp}
	password := auth.User{ID: 1, Role: auth.RoleAdmin, ElevatedUntil: 1 << 40, ElevatedBy: auth.ElevatedByPassword}
	full := auth.User{ID: 1, Role: auth.RoleAdmin, TokenID: 1, AllowWrite: true, AllowExec: true, AllowNodeExec: true, AllowPanel: true}
	for _, p := range passwordOnly {
		if !needsElevation(plain, p) {
			t.Errorf("%s：没做确认的会话应该要确认", p)
		}
		if auth.TokenAllowed(full, p) {
			t.Errorf("%s：访问令牌不能调用", p)
		}
		if !needsElevation(app, p) {
			t.Errorf("%s：手机上做的确认不算", p)
		}
		if needsElevation(password, p) {
			t.Errorf("%s：用密码确认过应该放行", p)
		}
	}
	if !readOnlyProcedure(nagiv1connect.AuthServiceElevateProcedure) {
		t.Error("备用面板上应放行高权限确认")
	}
}
