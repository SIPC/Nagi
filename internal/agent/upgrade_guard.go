package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/release"
)

// 升级后的自检（见 docs/control-plane.md#接口设计）：新版本试运行 version 通过，正式启动时却可能起不来（迁移节点数据出错、
// 启动时崩溃、连不上面板）。替换前旧版本留成 nagi.old（release.Prepared.Commit），并记下 upgrade-pending.json；
// 新版本连上面板才算升级完成，删掉这条记录。连续启动 upgradeMaxStarts 次都在 upgradeConfirmWindow 内退出、没连上
// （反复崩溃、systemd 一直在拉起），就换回旧版本重启。面板看到节点用旧版本回来，记为升级失败。
// 连不上面板不回滚，只记日志、继续重试：能挡住节点和面板之间连接的人，不能借此让节点退回有漏洞的旧版本。
// 这次启动撑过了 upgradeConfirmWindow 就不算崩溃，启动次数清零，之后连上面板时照常删掉记录。
const (
	upgradeConfirmWindow = 2 * time.Minute
	upgradeMaxStarts     = 3
)

type pendingUpgrade struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Starts int    `json:"starts"`
}

func pendingUpgradeFile(identityDir string) string {
	return filepath.Join(identityDir, "upgrade-pending.json")
}

// markUpgrade 在替换可执行文件以后、重启之前调用。
func markUpgrade(identityDir, from, to string) error {
	b, err := json.Marshal(pendingUpgrade{From: from, To: to})
	if err != nil {
		return err
	}
	return os.WriteFile(pendingUpgradeFile(identityDir), b, 0o600)
}

// guardUpgrade 在节点启动时最先调用。刚升级到 version 时开始自检，返回的 confirm 在连上面板后调用；
// 不是刚升级（没有记录，或者记录的目标版本不是现在这个）时 confirm 什么也不做。
func guardUpgrade(identityDir, version string, log *slog.Logger) (confirm func()) {
	return guardUpgradeWith(identityDir, version, log, upgradeConfirmWindow, rollbackUpgrade)
}

func guardUpgradeWith(identityDir, version string, log *slog.Logger, window time.Duration, rollback func(string, pendingUpgrade, *slog.Logger, string)) func() {
	file := pendingUpgradeFile(identityDir)
	b, err := os.ReadFile(file)
	if err != nil {
		return func() {}
	}
	var p pendingUpgrade
	if json.Unmarshal(b, &p) != nil || p.To != version {
		// 已经换回去了，或者在节点上重新执行了安装脚本。
		_ = os.Remove(file)
		return func() {}
	}
	p.Starts++
	if p.Starts > upgradeMaxStarts {
		rollback(file, p, log, "连续启动都没有连上面板")
		return func() {}
	}
	if b, err := json.Marshal(p); err == nil {
		_ = os.WriteFile(file, b, 0o600)
	}
	var mu sync.Mutex
	confirmed := false
	go func() {
		time.Sleep(window)
		mu.Lock()
		defer mu.Unlock()
		if confirmed {
			return
		}
		// 没有崩溃，只是还没连上面板：清零启动次数，等连上再算升级完成。
		log.Warn("新版本启动后 2 分钟内没有连上面板，继续重试", "version", version)
		p.Starts = 0
		if b, err := json.Marshal(p); err == nil {
			_ = os.WriteFile(file, b, 0o600)
		}
	}()
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if confirmed {
			return
		}
		confirmed = true
		_ = os.Remove(file)
		log.Info("新版本已连上面板，升级完成", "version", version)
	}
}

// rollbackUpgrade 换回上一个版本并原地重启。换不回去时只记日志，留着新版本继续尝试。
func rollbackUpgrade(file string, p pendingUpgrade, log *slog.Logger, reason string) {
	log.Error("新版本没能正常运行，换回上一个版本", "version", p.To, "previous", p.From, "reason", reason, "starts", p.Starts)
	exe, err := release.RestorePrevious()
	_ = os.Remove(file)
	if err != nil {
		log.Error("换回上一个版本失败，请在节点上重新执行安装脚本", "err", err)
		return
	}
	if err := release.Reexec(exe); err != nil {
		log.Error("换回上一个版本后重启失败，请手动重启 nagi", "err", err)
	}
}
