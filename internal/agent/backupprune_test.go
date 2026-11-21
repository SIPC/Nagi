package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 去掉备份节点时，到它正在进行（或排着队）的同步停下，也不再来一次；留下的备份节点、别的容器不受影响。
func TestBackupPruneCancelsRun(t *testing.T) {
	b := newBackupStates(filepath.Join(t.TempDir(), "backup-status.json"))
	ctx := context.Background()
	gone := b.claim(ctx, "p-web", "n2")
	kept := b.claim(ctx, "p-web", "n3")
	other := b.claim(ctx, "p-webx", "n2") // 名字以 p-web 开头的另一个容器
	b.claim(ctx, "p-web", "n2")           // 同步进行中又有人请求：标记再来一次
	if !gone.again {
		t.Fatal("应标记再来一次")
	}

	b.mu.Lock()
	removed := b.prune("p-web", []*nagiv1.BackupTarget{{NodeId: "n3"}})
	b.mu.Unlock()
	if !removed {
		t.Error("应报告去掉了")
	}
	if context.Cause(gone.ctx) != errSyncStopped || gone.again {
		t.Errorf("去掉的备份节点：cause=%v again=%v", context.Cause(gone.ctx), gone.again)
	}
	if kept.ctx.Err() != nil || other.ctx.Err() != nil {
		t.Error("留下的备份节点、别的容器不应停")
	}
	b.mu.Lock()
	if b.prune("p-web", []*nagiv1.BackupTarget{{NodeId: "n3"}}) {
		t.Error("没有要去掉的时应报告 false")
	}
	b.mu.Unlock()

	// 删除容器：先停下所有的同步（状态留着），删除成功后全部去掉。
	b.stopAll("p-web")
	if context.Cause(kept.ctx) != errSyncStopped || other.ctx.Err() != nil {
		t.Error("stopAll 只停这个容器的")
	}
	b.mu.Lock()
	if b.m["p-web"] == nil && b.m["p-web/n3"] == nil {
		t.Error("stopAll 不应去掉状态")
	}
	b.prune("p-web", nil)
	for k, e := range b.m {
		if k == "p-web" || k == "p-web/"+e.Target {
			t.Errorf("删除容器后状态还在：%s", k)
		}
	}
	if len(b.m) != 1 {
		t.Errorf("别的容器的状态：%v", b.m)
	}
	b.mu.Unlock()
}

// 排着队（等同一个容器前一个备份节点）的同步在备份节点被去掉后马上结束，不留在队里，结果不写回状态。
func TestBackupPruneQueuedRunEnds(t *testing.T) {
	logs, err := logstore.Open(t.Context(), filepath.Join(t.TempDir(), "logs.db"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a := &agent{cfg: Config{Logs: logs}, backups: newBackupStates(filepath.Join(t.TempDir(), "backup-status.json"))}
	unlock, err := lockSync(context.Background(), "backup/p-q")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	run := a.backups.claim(context.Background(), "p-q", "n2")
	done := make(chan struct{})
	go func() {
		a.syncBackupTo("p", "q", &nagiv1.BackupTarget{NodeId: "n2", Name: "n2"}, run)
		close(done)
	}()
	a.backups.mu.Lock()
	a.backups.prune("p-q", nil)
	a.backups.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("去掉备份节点后排队的同步没有结束")
	}
	select {
	case <-run.done:
	default:
		t.Error("run.done 应关闭")
	}
	a.backups.mu.Lock()
	defer a.backups.mu.Unlock()
	if len(a.backups.m) != 0 {
		t.Errorf("去掉的备份节点的状态被写回了：%v", a.backups.m)
	}
	rows, err := logs.Query(t.Context(), logstore.Filter{Category: logstore.Task})
	if err != nil || len(rows) != 1 || rows[0].Kind != "replica.queue" {
		t.Fatalf("取消排队也应保留结果、不能假装开始传输: %+v %v", rows, err)
	}
	var data struct{ Phase, Result string }
	if err := json.Unmarshal(rows[0].Data.(json.RawMessage), &data); err != nil || data.Phase != "finished" || data.Result != "stopped" {
		t.Fatalf("取消结果: %s %v", rows[0].Data, err)
	}
}
