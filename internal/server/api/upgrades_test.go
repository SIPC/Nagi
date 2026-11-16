package api

import (
	"strings"
	"testing"
	"time"
)

func TestUpdateRecordsResume(t *testing.T) {
	now := time.Unix(1000, 0)
	r := updateRecords{
		Panel: &updateState{Target: "v0.9.0", Phase: updateRestarting},
		Nodes: map[string]*updateState{
			"queued":     {Target: "v0.9.0", Phase: updateQueued},
			"installing": {Target: "v0.9.0", Phase: updateInstalling},
			"restarting": {Target: "v0.9.0", Phase: updateRestarting},
			"failed":     {Target: "v0.9.0", Phase: updateFailed, Error: "原来的原因"},
		},
	}
	// 重启后版本对上了：更新成功，记录删掉。
	r.resume("v0.9.0", now)
	if r.Panel != nil {
		t.Errorf("版本对上了应该删掉：%+v", r.Panel)
	}
	if _, ok := r.Nodes["queued"]; ok {
		t.Error("排队的节点后台任务已经没了，应该回到可以升级")
	}
	if n := r.Nodes["installing"]; n.Phase != updateFailed || n.Error == "" {
		t.Errorf("下载中被打断应该算失败：%+v", n)
	}
	if n := r.Nodes["restarting"]; n.Phase != updateRestarting {
		t.Errorf("节点已经在重启，面板重启后接着等：%+v", n)
	}
	if n := r.Nodes["failed"]; n.Error != "原来的原因" {
		t.Errorf("失败原因不应该变：%+v", n)
	}

	// 重启后版本没变。
	for phase, want := range map[string]string{updateRestarting: updateFailed, updateInstalling: updateFailed} {
		r := updateRecords{Panel: &updateState{Target: "v0.9.0", Phase: phase}}
		r.resume("v0.8.0", now)
		if r.Panel == nil || r.Panel.Phase != want || r.Panel.Error == "" {
			t.Errorf("%s：应该算失败：%+v", phase, r.Panel)
		}
	}
}

func TestUpdateRecordsReconcile(t *testing.T) {
	now := time.Unix(10000, 0)
	recent := now.Unix() - 10
	old := now.Unix() - int64(updateRestartTimeout/time.Second) - 1
	r := updateRecords{
		Panel: &updateState{Target: "v0.9.0", Phase: updateRestarting, Changed: old},
		Nodes: map[string]*updateState{
			"back":    {Target: "v0.9.0", Phase: updateRestarting, Changed: recent},
			"waiting": {Target: "v0.9.0", Phase: updateRestarting, Changed: recent},
			"lost":    {Target: "v0.9.0", Phase: updateRestarting, Changed: old},
			"late":    {Target: "v0.9.0", Phase: updateFailed, Changed: old},
			"removed": {Target: "v0.9.0", Phase: updateFailed, Changed: old},
			"queued":  {Target: "v0.9.0", Phase: updateQueued, Changed: old},
		},
	}
	versions := map[string]string{"back": "v0.9.0", "late": "v0.9.0", "queued": "v0.8.0"}
	r.reconcile("v0.8.0", func(id string) string { return versions[id] }, func(string) int64 { return 0 }, func(id string) bool { return id != "removed" }, now)
	if r.Panel == nil || r.Panel.Phase != updateFailed {
		t.Errorf("面板超时没恢复应该算失败：%+v", r.Panel)
	}
	for _, id := range []string{"back", "late", "removed"} {
		if _, ok := r.Nodes[id]; ok {
			t.Errorf("%s：记录应该删掉", id)
		}
	}
	if n := r.Nodes["waiting"]; n == nil || n.Phase != updateRestarting {
		t.Errorf("还没超时应该接着等：%+v", n)
	}
	if n := r.Nodes["lost"]; n == nil || n.Phase != updateFailed || n.Error == "" {
		t.Errorf("超时没回来应该算失败：%+v", n)
	}
	if n := r.Nodes["queued"]; n == nil || n.Phase != updateQueued {
		t.Errorf("排队的不受超时影响：%+v", n)
	}

	// 替换以后节点又起来了，版本还是旧的：新版本没能运行，节点自动换回去了，马上记为失败，不等 3 分钟。
	r = updateRecords{Nodes: map[string]*updateState{
		"rolledback": {Target: "v0.9.0", From: "v0.8.0", Phase: updateRestarting, Changed: recent},
		"restarting": {Target: "v0.9.0", From: "v0.8.0", Phase: updateRestarting, Changed: recent},
	}}
	started := map[string]int64{"rolledback": recent + 5, "restarting": recent - 100}
	r.reconcile("v0.9.0", func(string) string { return "v0.8.0" }, func(id string) int64 { return started[id] }, func(string) bool { return true }, now)
	if n := r.Nodes["rolledback"]; n == nil || n.Phase != updateFailed || !strings.Contains(n.Error, "自动回到 v0.8.0") {
		t.Errorf("节点换回了旧版本应该算失败：%+v", n)
	}
	if n := r.Nodes["restarting"]; n == nil || n.Phase != updateRestarting {
		t.Errorf("还是替换前的进程（没重启过）应该接着等：%+v", n)
	}

	// 面板用新版本回来了。
	r = updateRecords{Panel: &updateState{Target: "v0.9.0", Phase: updateInstalling}, Nodes: map[string]*updateState{}}
	r.reconcile("v0.9.0", func(string) string { return "" }, func(string) int64 { return 0 }, func(string) bool { return true }, now)
	if r.Panel != nil {
		t.Errorf("面板已经是目标版本，记录应该删掉：%+v", r.Panel)
	}
}
