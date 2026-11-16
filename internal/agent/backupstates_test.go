package agent

import (
	"path/filepath"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 每个备份节点各记各的同步状态；升级前只有一个备份节点时记的状态接得上；去掉的备份节点的状态清掉。
func TestBackupStatesPerTarget(t *testing.T) {
	file := filepath.Join(t.TempDir(), "backup-status.json")
	b := newBackupStates(file)
	// 升级前的记录：键只有"项目 ID-容器名"。
	b.m["p-web"] = &backupEntry{Target: "n2", LastSync: 100}
	target := func(id string) *nagiv1.BackupTarget { return &nagiv1.BackupTarget{NodeId: id, Name: id} }
	sp := &nagiv1.ContainerSpec{ProjectId: "p", Name: "web", Backups: []*nagiv1.BackupTarget{target("n2"), target("n3")}}

	b.mu.Lock()
	b.entry("p-web", "n3").LastError = "离线"
	b.persist()
	b.mu.Unlock()
	st := b.statuses(sp)
	if len(st) != 2 || st[0].GetTargetNodeId() != "n2" || st[0].GetLastSync().GetSeconds() != 100 || st[1].GetLastError() != "离线" {
		t.Fatalf("状态：%v", st)
	}
	b.mu.Lock()
	if f := b.failures("p-web", sp.GetBackups()); len(f) != 1 {
		t.Errorf("失败的备份节点：%v", f)
	}
	b.mu.Unlock()

	// 重新读文件，状态还在。
	b = newBackupStates(file)
	if st := b.statuses(sp); st[0].GetLastSync().GetSeconds() != 100 || st[1].GetLastError() != "离线" {
		t.Fatalf("重新读取后：%v", st)
	}

	// 去掉 n2：它的状态清掉，n3 的留着；之后再加一个 n4 是从头记的。
	sp.Backups = []*nagiv1.BackupTarget{target("n3"), target("n4")}
	b.mu.Lock()
	b.prune("p-web", sp.GetBackups())
	b.mu.Unlock()
	st = b.statuses(sp)
	if st[0].GetLastError() != "离线" || st[1].GetLastSync() != nil || st[1].GetLastError() != "" {
		t.Fatalf("去掉一个以后：%v", st)
	}
	for k, e := range b.m {
		if e.Target == "n2" {
			t.Errorf("去掉的备份节点的状态还在：%s", k)
		}
	}

	// 旧版本的面板只下发 backup 一个。
	old := &nagiv1.ContainerSpec{ProjectId: "p", Name: "web", Backup: target("n3")}
	if st := b.statuses(old); len(st) != 1 || st[0].GetTargetNodeId() != "n3" {
		t.Errorf("只有 backup 时：%v", st)
	}
	if b.statuses(&nagiv1.ContainerSpec{ProjectId: "p", Name: "web"}) != nil {
		t.Error("没有备份节点时应该是 nil")
	}
}
