package auth

import "testing"

func TestAllowed(t *testing.T) {
	const (
		deploy   = "/nagi.v1.ProjectService/Deploy"
		list     = "/nagi.v1.ProjectService/ListProjects"
		key      = "/nagi.v1.ProjectService/GetBackupKey"
		events   = "/nagi.v1.ProjectService/ListContainerEvents"
		volumes  = "/nagi.v1.ProjectService/ListVolumes"
		files    = "/nagi.v1.ProjectService/ListVolumeFiles"
		delFile  = "/nagi.v1.ProjectService/DeleteVolumeFile"
		settings = "/nagi.v1.SettingsService/UpdateSettings"
		users    = "/nagi.v1.SettingsService/ListUsers"
		alerts   = "/nagi.v1.SettingsService/GetAlertConfig"
		removeN  = "/nagi.v1.NodeService/RemoveNode"
		nodes    = "/nagi.v1.NodeService/ListNodes"
		password = "/nagi.v1.AuthService/ChangePassword"
		sessions = "/nagi.v1.SettingsService/RevokeSession"
		unknown  = "/nagi.v1.SettingsService/SomethingNew"
	)
	for _, c := range []struct {
		role, procedure string
		want            bool
	}{
		{RoleAdmin, deploy, true}, {RoleAdmin, settings, true}, {RoleAdmin, users, true}, {RoleAdmin, unknown, true},
		{RoleOperator, deploy, true}, {RoleOperator, list, true}, {RoleOperator, key, true}, {RoleOperator, nodes, true},
		{RoleOperator, settings, false}, {RoleOperator, users, false}, {RoleOperator, alerts, false}, {RoleOperator, removeN, false}, {RoleOperator, unknown, false},
		{RoleOperator, password, true}, {RoleOperator, sessions, true},
		{RoleOperator, events, true}, {RoleViewer, events, true},
		{RoleViewer, volumes, true}, {RoleViewer, files, false}, {RoleViewer, delFile, false},
		{RoleOperator, volumes, true}, {RoleOperator, files, true}, {RoleOperator, delFile, true},
		{RoleViewer, list, true}, {RoleViewer, nodes, true}, {RoleViewer, password, true}, {RoleViewer, sessions, true},
		{RoleViewer, deploy, false}, {RoleViewer, key, false}, {RoleViewer, settings, false}, {RoleViewer, users, false}, {RoleViewer, alerts, false}, {RoleViewer, removeN, false}, {RoleViewer, unknown, false},
		{"", list, false}, {"root", list, false},
	} {
		if got := Allowed(c.role, c.procedure); got != c.want {
			t.Errorf("Allowed(%q, %s) = %v，应为 %v", c.role, c.procedure, got, c.want)
		}
	}
}
