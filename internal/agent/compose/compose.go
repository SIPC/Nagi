// Package compose 调用 docker compose 命令部署项目。部署行为和用户手动执行 compose 完全一致。
package compose

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// Files 描述一次 compose 调用用到的文件。
type Files struct {
	ProjectName string // compose 项目名，例如 nagi-<项目 ID>-<容器>
	ProjectDir  string // compose 的项目目录，相对路径以它为准（容器的 data/）
	ComposeFile string // 用户的 compose 文件
	Override    string // Nagi 生成的叠加配置
	// EnvFiles 是替换 ${变量} 用的变量文件，按顺序传，后面的覆盖前面的（共享变量、容器变量）。
	EnvFiles []string
	// DockerConfig 是项目自己的 docker 配置目录（私有仓库登录信息），为空时用机器全局的配置。
	DockerConfig string
}

func (f Files) args(sub ...string) []string {
	args := []string{"compose",
		"-p", f.ProjectName,
		"--project-directory", f.ProjectDir,
		"-f", f.ComposeFile,
		"-f", f.Override,
	}
	for _, e := range f.EnvFiles {
		args = append(args, "--env-file", e)
	}
	return append(args, sub...)
}

// Up 执行 docker compose up -d --remove-orphans，每输出一行就调用一次 onLine。
func Up(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args("up", "-d", "--remove-orphans"), onLine)
}

// Recreate 强制重建指定服务的容器（不影响它依赖的服务）。
func Recreate(ctx context.Context, f Files, services []string, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args(append([]string{"up", "-d", "--force-recreate", "--no-deps"}, services...)...), onLine)
}

// Start 启动项目已有的容器（不重建）。
func Start(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args("start"), onLine)
}

// Stop 停止项目的所有容器（不删除）。
func Stop(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args("stop"), onLine)
}

// Restart 重启项目的所有容器（不重建）。
func Restart(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args("restart"), onLine)
}

// Down 停止并删除项目的容器和网络。removeVolumes 时一并删除命名 volume。
func Down(ctx context.Context, f Files, removeVolumes bool, onLine func(string)) error {
	args := []string{"down", "--remove-orphans"}
	if removeVolumes {
		args = append(args, "-v")
	}
	return run(ctx, f.DockerConfig, f.args(args...), onLine)
}

// Pull 拉取项目用到的镜像（本地构建的服务跳过）。
func Pull(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.DockerConfig, f.args("pull", "--ignore-buildable"), onLine)
}

// Version 返回 docker compose 插件的版本号，例如 "2.39.1"。
func Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Output()
	if err != nil {
		return "", fmt.Errorf("无法执行 docker compose：%w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), nil
}

// passEnv 是传给 docker 命令的节点进程环境变量：docker 命令本身要用的，其余的不传。
// compose 会用调用它的进程的环境变量替换 compose 里的 ${变量}、补上 environment 里只写了名字的变量，
// 而且优先于 --env-file；全部继承的话，节点进程环境里的值（例如 systemd 里配的令牌）就能被项目配置读到。
var passEnv = []string{"PATH", "HOME", "USER", "LANG", "TZ", "TMPDIR", "XDG_RUNTIME_DIR",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION"}

// baseEnv 返回 passEnv 和 LC_* 里节点进程设置了的那些。
func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(passEnv, k) || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

func run(ctx context.Context, dockerConfig string, args []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	// 不让 compose 输出颜色和进度动画，方便按行转发。
	cmd.Env = append(baseEnv(), "COMPOSE_ANSI=never", "COMPOSE_PROGRESS=plain", "NO_COLOR=1")
	if dockerConfig != "" {
		cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+dockerConfig)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法执行 docker compose：%w", err)
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		last string
	)
	forward := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			last = line
			onLine(line)
			mu.Unlock()
		}
	}
	wg.Add(2)
	go forward(stdout)
	go forward(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && last != "" {
			return fmt.Errorf("docker compose 失败：%s", last)
		}
		return fmt.Errorf("docker compose 失败：%w", err)
	}
	return nil
}

// Login 用 docker login 验证账号并把登录信息写进 dockerConfig 目录（不碰机器全局的 ~/.docker）。
func Login(ctx context.Context, dockerConfig, registry, username, password string) error {
	cmd := exec.CommandContext(ctx, "docker", "login", "--username", username, "--password-stdin", registry)
	cmd.Env = append(baseEnv(), "DOCKER_CONFIG="+dockerConfig)
	cmd.Stdin = strings.NewReader(password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("登录 %s 失败：%s", registry, strings.TrimSpace(string(out)))
	}
	return nil
}

// Logout 删除 dockerConfig 里某个仓库的登录信息。
func Logout(ctx context.Context, dockerConfig, registry string) error {
	cmd := exec.CommandContext(ctx, "docker", "logout", registry)
	cmd.Env = append(baseEnv(), "DOCKER_CONFIG="+dockerConfig)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
