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
	"strings"
	"sync"
)

// Files 描述一次 compose 调用用到的文件。
type Files struct {
	ProjectName string // compose 项目名，例如 nagi-<项目 ID>
	ProjectDir  string // compose 的项目目录，相对路径以它为准（项目的 data/）
	ComposeFile string // 用户的 compose 文件
	Override    string // Nagi 生成的叠加配置
	EnvFile     string // 项目级变量
}

func (f Files) args(sub ...string) []string {
	args := []string{"compose",
		"-p", f.ProjectName,
		"--project-directory", f.ProjectDir,
		"-f", f.ComposeFile,
		"-f", f.Override,
		"--env-file", f.EnvFile,
	}
	return append(args, sub...)
}

// Up 执行 docker compose up -d --remove-orphans，每输出一行就调用一次 onLine。
func Up(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.args("up", "-d", "--remove-orphans"), onLine)
}

// Recreate 强制重建指定服务的容器（不影响它依赖的服务）。
func Recreate(ctx context.Context, f Files, services []string, onLine func(string)) error {
	return run(ctx, f.args(append([]string{"up", "-d", "--force-recreate", "--no-deps"}, services...)...), onLine)
}

// Stop 停止项目的所有容器（不删除）。
func Stop(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.args("stop"), onLine)
}

// Down 停止并删除项目的容器和网络。removeVolumes 时一并删除命名 volume。
func Down(ctx context.Context, f Files, removeVolumes bool, onLine func(string)) error {
	args := []string{"down", "--remove-orphans"}
	if removeVolumes {
		args = append(args, "-v")
	}
	return run(ctx, f.args(args...), onLine)
}

// Pull 拉取项目用到的镜像（本地构建的服务跳过）。
func Pull(ctx context.Context, f Files, onLine func(string)) error {
	return run(ctx, f.args("pull", "--ignore-buildable"), onLine)
}

// Version 返回 docker compose 插件的版本号，例如 "2.39.1"。
func Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Output()
	if err != nil {
		return "", fmt.Errorf("无法执行 docker compose：%w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), nil
}

func run(ctx context.Context, args []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	// 不让 compose 输出颜色和进度动画，方便按行转发。
	cmd.Env = append(os.Environ(), "COMPOSE_ANSI=never", "COMPOSE_PROGRESS=plain", "NO_COLOR=1")

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
