package projects

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// L15：被攻破的节点 X 上报别的节点（Y）的容器的同步状态，之前会覆盖 Y 离线前留下的记录，从备份恢复时可能选到更旧的副本。
// 现在只记面板上记着在上报节点上的容器。
func TestRememberBackupsOnlyOwnContainers(t *testing.T) {
	s := &Service{}
	key := "p1/db"
	legit := []*nagiv1.ContainerStatus{{ProjectId: "p1", Name: "db", Backups: []*nagiv1.BackupStatus{{TargetNodeId: "b1"}}}}
	s.rememberBackups("y", legit, map[string]bool{key: true})
	forged := []*nagiv1.ContainerStatus{{ProjectId: "p1", Name: "db", Backups: []*nagiv1.BackupStatus{{TargetNodeId: "b2"}}}}
	s.rememberBackups("x", forged, map[string]bool{})
	if r := s.backupReports[key]; r.node != "y" || len(r.states) != 1 || r.states[0].GetTargetNodeId() != "b1" {
		t.Fatalf("别的节点上报的容器不应覆盖记录：%+v", r)
	}
	// X 上报一个空列表，也删不掉 Y 的记录。
	s.rememberBackups("x", []*nagiv1.ContainerStatus{{ProjectId: "p1", Name: "db"}}, map[string]bool{})
	if s.backupReports[key].node != "y" {
		t.Fatal("别的节点不能删掉记录")
	}
}
