package store

import "strings"

// 容器的备份节点存在 containers.backup_node_id 这一列里，有多个时用逗号隔开，按添加的顺序（见 docs/snapshot.md#异地副本）。
// 以前只能有一个备份节点，沿用这一列不用迁移数据；节点 ID 是十六进制，不含逗号。

// BackupNodes 返回容器的备份节点，没有时返回 nil。
func BackupNodes(c Container) []string {
	return SplitBackupNodes(c.BackupNodeID)
}

func SplitBackupNodes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func JoinBackupNodes(ids []string) string {
	return strings.Join(ids, ",")
}
