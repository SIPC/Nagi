package agent

import (
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

// 升级后的自检：连上面板就删掉记录；反复崩溃时第 4 次启动直接换回去；不是刚升级的版本不管。
// N3：连不上面板不换回（能挡住连接的人不能借此让节点退回旧版本），撑过窗口的启动不算崩溃。
func TestGuardUpgrade(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	type call struct{ reason string }
	newRollback := func() (func() []call, func(string, pendingUpgrade, *slog.Logger, string)) {
		var mu sync.Mutex
		calls := []call{}
		get := func() []call {
			mu.Lock()
			defer mu.Unlock()
			return append([]call(nil), calls...)
		}
		return get, func(file string, _ pendingUpgrade, _ *slog.Logger, reason string) {
			mu.Lock()
			defer mu.Unlock()
			_ = os.Remove(file)
			calls = append(calls, call{reason})
		}
	}

	t.Run("连上面板", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		confirm := guardUpgradeWith(dir, "v2", log, 50*time.Millisecond, rb)
		confirm()
		time.Sleep(100 * time.Millisecond)
		if len(calls()) != 0 {
			t.Fatalf("连上面板后不应换回：%v", calls())
		}
		if _, err := os.Stat(pendingUpgradeFile(dir)); !os.IsNotExist(err) {
			t.Fatal("记录应删掉")
		}
	})

	t.Run("连不上面板", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		guardUpgradeWith(dir, "v2", log, 20*time.Millisecond, rb)
		time.Sleep(100 * time.Millisecond)
		if len(calls()) != 0 {
			t.Fatalf("连不上面板不应换回：%v", calls())
		}
		// 撑过窗口的启动不算崩溃：之后再重启几次（例如面板一直不可用、机器重启）也不换回。
		for range upgradeMaxStarts + 2 {
			guardUpgradeWith(dir, "v2", log, 10*time.Millisecond, rb)
			time.Sleep(50 * time.Millisecond)
		}
		if len(calls()) != 0 {
			t.Fatalf("没有崩溃时不应换回：%v", calls())
		}
		// 记录还在：之后连上面板才算升级完成。
		confirm := guardUpgradeWith(dir, "v2", log, time.Hour, rb)
		if _, err := os.Stat(pendingUpgradeFile(dir)); err != nil {
			t.Fatal("没连上面板之前记录应保留")
		}
		confirm()
		if _, err := os.Stat(pendingUpgradeFile(dir)); !os.IsNotExist(err) {
			t.Fatal("连上后记录应删掉")
		}
	})

	t.Run("反复崩溃", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		// 前 3 次启动都在连上面板之前崩溃（窗口设长，不让计时器触发）。
		for range upgradeMaxStarts {
			guardUpgradeWith(dir, "v2", log, time.Hour, rb)
		}
		if len(calls()) != 0 {
			t.Fatalf("前 %d 次启动不应换回：%v", upgradeMaxStarts, calls())
		}
		guardUpgradeWith(dir, "v2", log, time.Hour, rb)
		if len(calls()) != 1 {
			t.Fatalf("第 %d 次启动应直接换回：%v", upgradeMaxStarts+1, calls())
		}
	})

	t.Run("不是刚升级的版本", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		// 已经换回 v1（或者在节点上重新安装了别的版本）：删掉记录，不做自检。
		guardUpgradeWith(dir, "v1", log, 20*time.Millisecond, rb)
		time.Sleep(60 * time.Millisecond)
		if len(calls()) != 0 {
			t.Fatalf("不应换回：%v", calls())
		}
		if _, err := os.Stat(pendingUpgradeFile(dir)); !os.IsNotExist(err) {
			t.Fatal("记录应删掉")
		}
	})
}
