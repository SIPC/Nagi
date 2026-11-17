package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// dockerReloader 让 dockerd 重新加载 daemon.json（向它发 SIGHUP）：registry-mirrors 这类配置不用重启 Docker 就能生效，
// 容器不受影响。Nagi 不重启 Docker。几个系统操作做成字段，测试里换掉。
type dockerReloader struct {
	// systemd 判断本机由 systemd 管理、有 systemctl。
	systemd func() bool
	// run 执行一条命令。
	run func(ctx context.Context, name string, args ...string) error
	// pidFile 读 dockerd 的 pid 文件。
	pidFile func() (int, bool)
	// comm 返回进程的名字（/proc/<pid>/comm），读不到时为空。
	comm func(pid int) string
	// pids 列出所有进程。
	pids func() []int
	// hup 向进程发 SIGHUP。
	hup func(pid int) error
}

// hostDockerReloader 是本机用的 dockerReloader。
var hostDockerReloader = dockerReloader{
	systemd: func() bool {
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			return false
		}
		_, err := exec.LookPath("systemctl")
		return err == nil
	},
	run: func(ctx context.Context, name string, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).Run()
	},
	pidFile: func() (int, bool) {
		for _, f := range []string{"/var/run/docker.pid", "/run/docker.pid"} {
			if b, err := os.ReadFile(f); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
					return pid, true
				}
			}
		}
		return 0, false
	},
	comm: func(pid int) string {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	},
	pids: func() []int {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil
		}
		var out []int
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
				out = append(out, pid)
			}
		}
		return out
	},
	hup: func(pid int) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Signal(syscall.SIGHUP)
	},
}

// reload 让 dockerd 重新加载配置，返回用的方法（给日志看）：
//  1. 由 systemd 管理、docker 服务在运行时用 systemctl reload docker（docker.service 的 ExecReload 就是发 SIGHUP）；
//  2. 否则（或者它失败了）按 pid 文件找到 dockerd 发 SIGHUP，先核对这个进程确实是 dockerd（pid 文件可能是旧的）；
//  3. 再不行在所有进程里找名为 dockerd 的，只有一个时发 SIGHUP。
func (r dockerReloader) reload(ctx context.Context) (string, error) {
	if r.systemd() && r.run(ctx, "systemctl", "is-active", "--quiet", "docker") == nil {
		if err := r.run(ctx, "systemctl", "reload", "docker"); err == nil {
			return "systemctl reload docker", nil
		}
	}
	if pid, ok := r.pidFile(); ok && r.comm(pid) == "dockerd" {
		if err := r.hup(pid); err != nil {
			return "", i18n.Detail(i18n.New("无法让 Docker 重新加载配置"), err)
		}
		return "SIGHUP " + strconv.Itoa(pid), nil
	}
	var found []int
	for _, pid := range r.pids() {
		if r.comm(pid) == "dockerd" {
			found = append(found, pid)
		}
	}
	switch len(found) {
	case 0:
		return "", i18n.New("找不到 dockerd 进程，无法让 Docker 重新加载配置")
	case 1:
		if err := r.hup(found[0]); err != nil {
			return "", i18n.Detail(i18n.New("无法让 Docker 重新加载配置"), err)
		}
		return "SIGHUP " + strconv.Itoa(found[0]), nil
	}
	return "", i18n.New("本机有多个 dockerd 进程，无法确定要重新加载哪一个")
}

// trimMirror 去掉末尾的 /：Docker 会给加速地址补上，比较时忽略。
func trimMirror(s string) string { return strings.TrimRight(s, "/") }

// sameMirrors 判断两组加速地址是否相同（顺序也要一样，Docker 按顺序使用）。
func sameMirrors(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if trimMirror(a[i]) != trimMirror(b[i]) {
			return false
		}
	}
	return true
}

// mirrorsReloadWait 是让 Docker 重新加载配置以后，等它用上新的加速地址的时间上限。
var mirrorsReloadWait = 5 * time.Second

// applyMirrors 让 Docker 重新加载配置，等它用上 want（daemon.json 里的加速地址）。返回 Docker 当前使用的地址；
// 没能生效时返回原因，这时要重启 Docker 才生效。
func (a *agent) applyMirrors(ctx context.Context, r dockerReloader, want []string) ([]string, error) {
	if a.docker == nil {
		return nil, i18n.New("无法连接 Docker，请重启 Docker 使加速地址生效")
	}
	if active, err := a.docker.Mirrors(ctx); err == nil && sameMirrors(active, want) {
		return active, nil
	}
	how, err := r.reload(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(mirrorsReloadWait)
	for {
		active, err := a.docker.Mirrors(ctx)
		if err == nil && sameMirrors(active, want) {
			a.log.Info("Docker 已重新加载配置", "method", how, "mirrors", active)
			return active, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return active, i18n.New("Docker 重新加载配置后仍未使用新的加速地址，请在节点上查看 Docker 日志，或重启 Docker")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
