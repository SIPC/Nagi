package api

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

func TestActivityRecorded(t *testing.T) {
	for procedure, want := range map[string]bool{
		nagiv1connect.ProjectServiceDeployProcedure:           true,
		nagiv1connect.ProjectServiceControlContainerProcedure: true,
		nagiv1connect.AuthServiceLoginProcedure:               true,
		nagiv1connect.ProjectServiceListProjectsProcedure:     false,
		nagiv1connect.ProjectServiceGetProjectProcedure:       false,
		nagiv1connect.AuthServiceMeProcedure:                  false,
		nagiv1connect.SettingsServiceListActivityProcedure:    false,
	} {
		if got := activityRecorded(procedure); got != want {
			t.Errorf("%s: 记录 = %v，应为 %v", procedure, got, want)
		}
	}
}

// 记录只取固定字段：登录请求里只有用户名，密码读不到。
func TestActivityFieldNeverReadsSecrets(t *testing.T) {
	req := &nagiv1.LoginRequest{Username: "admin", Password: "secret"}
	if got := activityField(req, "username"); got != "admin" {
		t.Errorf("用户名 = %q", got)
	}
	if got := activityField(req, "project_id", "container", "name", "node_id", "id"); got != "" {
		t.Errorf("目标 = %q，应为空", got)
	}
}

func TestActivityFieldFirstMatch(t *testing.T) {
	req := &nagiv1.ControlContainerRequest{ProjectId: "p1", Container: "web"}
	if got := activityField(req, "project_id"); got != "p1" {
		t.Errorf("项目 = %q", got)
	}
	if got := activityField(req, "container", "name"); got != "web" {
		t.Errorf("目标 = %q", got)
	}
}
