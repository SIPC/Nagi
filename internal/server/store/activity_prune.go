package store

import (
	"context"
	"strings"
)

// SensitiveActions 是安全相关的活动记录：在节点或容器里执行命令、高权限确认、账号和凭据的变更、备用面板。
// 它们不跟普通记录一起按条数轮换（见 PruneActivityKeepSensitive），免得被大量普通记录挤掉。
var SensitiveActions = []string{
	"NodeExec", "OpenNodeShell", "CloseNodeShell", "ExecCommand", "OpenTerminal", "MCPToolDenied",
	"Elevate", "ChangePassword", "BeginTOTP", "EnableTOTP", "DisableTOTP",
	"CreateUser", "UpdateUser", "DeleteUser", "CreateAccessToken", "RevokeAccessToken",
	"CreateStandbyCode", "RemoveStandby", "PromoteStandby", "ConfirmPendingHA",
	// 节点上的文件（等于节点的 root）、安全入口、节点加入和移交。
	"DownloadNodeFile", "UploadNodeFile", "ReadNodeFile", "WriteNodeFile", "DeleteNodeFile", "CreateNodeDir", "RestoreNodeFileBackup",
	"SetEntrance", "CreateEnrollCommand", "ApproveNode", "RemoveNode", "HandoverNodes",
	// 分组和标签决定成员的访问范围。
	"SetProjectGroup", "RenameProjectGroup", "SetNodeTags", "RenameNodeTag",
}

func sensitiveList() string {
	return "'" + strings.Join(SensitiveActions, "','") + "'"
}

// PruneActivityKeepSensitive 删掉旧的活动记录：普通记录保留最新的 keep 条，安全相关的记录另外保留最新的 keep 条。
func (q *Queries) PruneActivityKeepSensitive(ctx context.Context, keep int64) error {
	list := sensitiveList()
	if _, err := q.db.ExecContext(ctx, `DELETE FROM activity WHERE action NOT IN (`+list+`) AND id <= (
		SELECT id FROM activity WHERE action NOT IN (`+list+`) ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `DELETE FROM activity WHERE action IN (`+list+`) AND id <= (
		SELECT id FROM activity WHERE action IN (`+list+`) ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
	return err
}
