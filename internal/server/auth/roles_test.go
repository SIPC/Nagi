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
		profile  = "/nagi.v1.AuthService/UpdateProfile"
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
		{RoleOperator, profile, true}, {RoleViewer, profile, true},
		{RoleViewer, list, true}, {RoleViewer, nodes, true}, {RoleViewer, password, true}, {RoleViewer, sessions, true},
		{RoleViewer, deploy, false}, {RoleViewer, key, false}, {RoleViewer, settings, false}, {RoleViewer, users, false}, {RoleViewer, alerts, false}, {RoleViewer, removeN, false}, {RoleViewer, unknown, false},
		{RoleViewer, "/nagi.v1.SettingsService/AddPasskey", true}, {RoleOperator, "/nagi.v1.SettingsService/DeletePasskey", true},
		{RoleViewer, "/nagi.v1.SettingsService/BeginPasskeyRegistration", true},
		{"", list, false}, {"root", list, false},
		// 节点访问只给管理员，查询也一样：名字以 List 开头的默认会放行。
		{RoleAdmin, "/nagi.v1.NodeService/ListNodeDir", true}, {RoleAdmin, "/nagi.v1.NodeService/ReadNodeFile", true},
		// 副本的存放位置只给管理员（见 docs/snapshot.md#副本的存放位置）；操作员能设置备份节点，存放位置在接口里另外拦。
		{RoleAdmin, "/nagi.v1.ProjectService/SetBackupReplicaDir", true}, {RoleAdmin, "/nagi.v1.NodeService/SetNodeReplicaDir", true},
		{RoleOperator, "/nagi.v1.ProjectService/SetBackupReplicaDir", false}, {RoleViewer, "/nagi.v1.ProjectService/SetBackupReplicaDir", false},
		{RoleOperator, "/nagi.v1.NodeService/SetNodeReplicaDir", false}, {RoleViewer, "/nagi.v1.NodeService/SetNodeReplicaDir", false},
		{RoleOperator, "/nagi.v1.ProjectService/SetBackupNodes", true},
	} {
		if got := Allowed(c.role, c.procedure); got != c.want {
			t.Errorf("Allowed(%q, %s) = %v，应为 %v", c.role, c.procedure, got, c.want)
		}
	}
}

// 单独控制一个服务和控制整个容器的权限一样：管理员、操作员可以，只读不行；访问令牌要勾「可修改」。
func TestControlServiceLikeControlContainer(t *testing.T) {
	for _, procedure := range []string{"/nagi.v1.ProjectService/ControlContainer", "/nagi.v1.ProjectService/ControlService"} {
		for role, want := range map[string]bool{RoleAdmin: true, RoleOperator: true, RoleViewer: false} {
			if got := Allowed(role, procedure); got != want {
				t.Errorf("Allowed(%q, %s) = %v，应为 %v", role, procedure, got, want)
			}
		}
		if TokenAllowed(User{ID: 1, Role: RoleOperator, TokenID: 1}, procedure) {
			t.Errorf("只读令牌不能调用 %s", procedure)
		}
		if !TokenAllowed(User{ID: 1, Role: RoleOperator, TokenID: 1, AllowWrite: true}, procedure) {
			t.Errorf("可修改的令牌应能调用 %s", procedure)
		}
	}
}

// 节点镜像只给管理员（见 docs/agent.md#节点镜像）。访问令牌：查看不用额外权限，删除、清理、拉取要勾「可修改」。
func TestNodeImagesAdminOnly(t *testing.T) {
	read := User{ID: 1, Role: RoleAdmin, TokenID: 1}
	write := User{ID: 1, Role: RoleAdmin, TokenID: 1, AllowWrite: true}
	for _, method := range []string{"ListNodeImages", "DeleteNodeImages", "PruneNodeImages", "PullNodeImage"} {
		p := "/nagi.v1.NodeService/" + method
		if !Allowed(RoleAdmin, p) {
			t.Errorf("管理员应能调用 %s", method)
		}
		for _, role := range []string{RoleOperator, RoleViewer} {
			if Allowed(role, p) {
				t.Errorf("%s 不应该能调用 %s", role, method)
			}
		}
		if !TokenAllowed(write, p) {
			t.Errorf("可修改的令牌应能调用 %s", method)
		}
		if got, want := TokenAllowed(read, p), method == "ListNodeImages"; got != want {
			t.Errorf("只读令牌调用 %s = %v，应为 %v", method, got, want)
		}
	}
}

// 新增节点访问接口时加到这里：操作员和只读账号都不能调用。
func TestNodeAccessAdminOnly(t *testing.T) {
	for _, method := range []string{
		"ListNodeDir", "ReadNodeFile", "WriteNodeFile", "DeleteNodeFile", "CreateNodeDir", "ListNodeFileBackups", "RestoreNodeFileBackup",
		"RenameNodeFile", "MoveNodeFiles", "CopyNodeFiles", "CompressNodeFiles", "ExtractNodeFile", "ListNodeDirSizes",
	} {
		for _, role := range []string{RoleOperator, RoleViewer} {
			if Allowed(role, "/nagi.v1.NodeService/"+method) {
				t.Errorf("%s 不应该能调用 %s", role, method)
			}
		}
	}
}
