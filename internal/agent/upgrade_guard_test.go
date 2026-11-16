package agent

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

// 升级后的自检：连上面板就删掉记录；没连上就换回去；反复崩溃时第 4 次启动直接换回去；不是刚升级的版本不管。
func TestGuardUpgrade(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	type call struct{ reason string }
	newRollback := func() (*[]call, func(string, pendingUpgrade, *slog.Logger, string)) {
		calls := &[]call{}
		return calls, func(file string, _ pendingUpgrade, _ *slog.Logger, reason string) {
			_ = os.Remove(file)
			*calls = append(*calls, call{reason})
		}
	}

	t.Run("连上面板", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		confirm := guardUpgradeWith(dir, "v2", log, 50*time.Millisecond, rb)
		confirm()
		time.Sleep(100 * time.Millisecond)
		if len(*calls) != 0 {
			t.Fatalf("连上面板后不应换回：%v", *calls)
		}
		if _, err := os.Stat(pendingUpgradeFile(dir)); !os.IsNotExist(err) {
			t.Fatal("记录应删掉")
		}
	})

	t.Run("没连上面板", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		guardUpgradeWith(dir, "v2", log, 20*time.Millisecond, rb)
		time.Sleep(100 * time.Millisecond)
		if len(*calls) != 1 {
			t.Fatalf("超时没连上应换回一次：%v", *calls)
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
		if len(*calls) != 0 {
			t.Fatalf("前 %d 次启动不应换回：%v", upgradeMaxStarts, *calls)
		}
		guardUpgradeWith(dir, "v2", log, time.Hour, rb)
		if len(*calls) != 1 {
			t.Fatalf("第 %d 次启动应直接换回：%v", upgradeMaxStarts+1, *calls)
		}
	})

	t.Run("不是刚升级的版本", func(t *testing.T) {
		dir := t.TempDir()
		_ = markUpgrade(dir, "v1", "v2")
		calls, rb := newRollback()
		// 已经换回 v1（或者在节点上重新安装了别的版本）：删掉记录，不做自检。
		guardUpgradeWith(dir, "v1", log, 20*time.Millisecond, rb)
		time.Sleep(60 * time.Millisecond)
		if len(*calls) != 0 {
			t.Fatalf("不应换回：%v", *calls)
		}
		if _, err := os.Stat(pendingUpgradeFile(dir)); !os.IsNotExist(err) {
			t.Fatal("记录应删掉")
		}
	})
}
