package store

import (
	"slices"
	"testing"
)

func TestBackupNodesColumn(t *testing.T) {
	if got := BackupNodes(Container{}); got != nil {
		t.Errorf("没有备份节点：%v", got)
	}
	if got := BackupNodes(Container{BackupNodeID: "n1"}); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("升级前的一个备份节点：%v", got)
	}
	ids := []string{"a1b2c3d4e5f60718", "0011223344556677"}
	if got := SplitBackupNodes(JoinBackupNodes(ids)); !slices.Equal(got, ids) {
		t.Errorf("往返：%v", got)
	}
	if JoinBackupNodes(nil) != "" {
		t.Error("没有备份节点时应该是空的")
	}
}
