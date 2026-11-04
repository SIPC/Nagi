//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// shellSession 是节点上的一个 shell。
type shellSession struct {
	cmd  *exec.Cmd
	pty  *os.File
	once sync.Once
	done chan struct{}
	code int
}

const shellPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// loginShell 找当前用户的登录 shell，没有时依次试 bash、sh。
func loginShell(u *user.User) string {
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, ":")
			if len(f) >= 7 && f[0] == u.Username && isExecutable(f[6]) && !strings.HasSuffix(f[6], "nologin") && !strings.HasSuffix(f[6], "false") {
				return f[6]
			}
		}
	}
	for _, s := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if isExecutable(s) {
			return s
		}
	}
	return "/bin/sh"
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

var (
	scopeOnce sync.Once
	scopeArgs []string
)

// systemdScope 返回把 shell 放进单独 scope 的命令前缀，不可用时为空。
// 否则 shell 和节点进程在同一个 cgroup 里，systemctl restart nagi-agent 或者升级时，在 shell 里启动的 tmux 会被一起杀掉。
func systemdScope() []string {
	scopeOnce.Do(func() {
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			return
		}
		run, err := exec.LookPath("systemd-run")
		if err != nil {
			return
		}
		args := []string{run, "--scope", "--quiet", "--collect", "--description=Nagi node shell", "--"}
		// 总线不通时 systemd-run 会直接失败，先试一次。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probe := exec.CommandContext(ctx, args[0], append(args[1:], "/bin/true")...)
		if probe.Run() == nil {
			scopeArgs = args
		}
	})
	return scopeArgs
}

// shellCommand 建一个节点上的 shell 命令：root 的登录 shell，放进单独的 scope，不继承节点进程的环境变量。
func shellCommand(ctx context.Context, cwd, term string, args ...string) *exec.Cmd {
	// 查不到用户时（容器里没有登记的 uid）照常打开，只是没有用户名和主目录。
	u, err := user.Current()
	if err != nil {
		u = &user.User{Username: strconv.Itoa(os.Getuid()), HomeDir: "/"}
	}
	shell := loginShell(u)
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	argv := append(append(systemdScope(), shell), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{
		"PATH=" + shellPath, "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username, "SHELL=" + shell,
		"TERM=" + term, "LANG=" + lang, "NAGI_NODE_SHELL=1",
	}
	cmd.Dir = u.HomeDir
	if info, err := os.Stat(cwd); cwd != "" && filepath.IsAbs(cwd) && err == nil && info.IsDir() {
		cmd.Dir = cwd
	}
	return cmd
}

// startShell 启动节点上的交互式 shell。
func startShell(cwd string, cols, rows uint16) (*shellSession, error) {
	cmd := shellCommand(context.Background(), cwd, "xterm-256color", "-l")
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	s := &shellSession{cmd: cmd, pty: f, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			s.code = ee.ExitCode()
		} else if err != nil {
			s.code = -1
		}
		close(s.done)
	}()
	return s, nil
}

func (s *shellSession) Read(p []byte) (int, error)  { return s.pty.Read(p) }
func (s *shellSession) Write(p []byte) (int, error) { return s.pty.Write(p) }

func (s *shellSession) Resize(cols, rows uint16) {
	if cols > 0 && rows > 0 {
		_ = pty.Setsize(s.pty, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

// Wait 等 shell 结束，返回退出码。读到 PTY 结束时 shell 通常已经退出了，再多等也没有意义。
func (s *shellSession) Wait() int {
	select {
	case <-s.done:
		return s.code
	case <-time.After(3 * time.Second):
		return -1
	}
}

// Close 结束 shell：关掉 PTY 发 SIGHUP，再给前台进程组发一次，3 秒没退就强制结束。
// 用 tmux、nohup 放到后台的进程不在这个进程组里，留着。
func (s *shellSession) Close() {
	s.once.Do(func() {
		_ = s.pty.Close()
		pid := s.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGHUP)
		go func() {
			select {
			case <-s.done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}()
	})
}

// runNodeCommand 在节点上执行一条命令（MCP 的 node_exec）：shell -lc，stdout 和 stderr 合在一起交给 out，没有输入。
// ctx 结束时杀掉整个进程组（包括命令启动的子进程），返回 -1。
func runNodeCommand(ctx context.Context, cwd, command string, out io.Writer) int {
	cmd := shellCommand(ctx, cwd, "dumb", "-lc", command)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// 后台进程拿着输出不放时，不要一直等。
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return -1
	case errors.As(err, &ee):
		return ee.ExitCode()
	case err != nil:
		fmt.Fprintf(out, "nagi: %v\n", err)
		return -1
	}
	return 0
}
