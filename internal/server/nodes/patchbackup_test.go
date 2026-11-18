package nodes

import (
	"log/slog"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 「立即同步」后节点返回的状态先合进最近一次上报：只换对应的备份节点，换成新的上报（旧的不被改），并通知订阅流。
func TestPatchBackup(t *testing.T) {
	r := NewRegistry(nil, slog.New(slog.DiscardHandler), nil, nil)
	old := &nagiv1.ReportRequest{Containers: []*nagiv1.ContainerStatus{
		{ProjectId: "p", Name: "other", Backups: []*nagiv1.BackupStatus{{TargetNodeId: "b1"}}},
		{ProjectId: "p", Name: "db", Backups: []*nagiv1.BackupStatus{{TargetNodeId: "b1"}, {TargetNodeId: "b2"}}},
	}}
	r.mu.Lock()
	r.conns["n1"] = &Conn{NodeID: "n1", report: old}
	r.mu.Unlock()
	wait := r.Changes.Wait()

	r.PatchBackup("n1", "p", "db", &nagiv1.BackupStatus{TargetNodeId: "b2", Syncing: true})
	got := r.conns["n1"].LastReport()
	if got == old {
		t.Fatal("应该换成新的上报")
	}
	if old.Containers[1].Backups[1].GetSyncing() {
		t.Fatal("旧的上报被改了")
	}
	db := got.Containers[1].Backups
	if !db[1].GetSyncing() || db[0].GetSyncing() || got.Containers[0].Backups[0].GetSyncing() {
		t.Fatalf("只应改 db 到 b2 的状态：%v", got.Containers)
	}
	select {
	case <-wait:
	default:
		t.Fatal("没有通知订阅流")
	}

	// 不认识的备份节点、容器、节点，以及空的状态：什么都不改。
	before := r.conns["n1"].LastReport()
	r.PatchBackup("n1", "p", "db", &nagiv1.BackupStatus{TargetNodeId: "b9", Syncing: true})
	r.PatchBackup("n1", "p", "nope", &nagiv1.BackupStatus{TargetNodeId: "b1", Syncing: true})
	r.PatchBackup("n1", "p", "db", nil)
	r.PatchBackup("missing", "p", "db", &nagiv1.BackupStatus{TargetNodeId: "b1"})
	if r.conns["n1"].LastReport() != before {
		t.Fatal("不该改上报")
	}
}
