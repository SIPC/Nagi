package compose

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestWithServices(t *testing.T) {
	if got := withServices([]string{"stop"}, nil); !slices.Equal(got, []string{"stop"}) {
		t.Errorf("没有服务：%q", got)
	}
	got := withServices([]string{"up", "-d", "--no-deps"}, []string{"-pother", "web"})
	if !slices.Equal(got, []string{"up", "-d", "--no-deps", "--", "-pother", "web"}) {
		t.Errorf("服务名前要有 --：%q", got)
	}
}

// 以 "-" 开头的服务名跟在 "--" 后面，docker compose 不会把它当成选项。用一个假的 docker 记下参数。
func TestServiceCommandsEndOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假的 docker 是 shell 脚本")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	f := Files{ProjectName: "nagi-p-c", ProjectDir: "/d", ComposeFile: "/d/compose.yaml", Override: "/d/o.yaml"}
	svcs := []string{"--remove-orphans", "web"}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"start":      func() error { return Start(ctx, f, func(string) {}, svcs...) },
		"stop":       func() error { return Stop(ctx, f, func(string) {}, svcs...) },
		"restart":    func() error { return Restart(ctx, f, func(string) {}, svcs...) },
		"up":         func() error { return UpServices(ctx, f, svcs, func(string) {}) },
		"recreate":   func() error { return Recreate(ctx, f, svcs, func(string) {}) },
		"stop (all)": func() error { return Stop(ctx, f, func(string) {}) },
	} {
		if err := call(); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		i := slices.Index(args, "--")
		if name == "stop (all)" {
			if i >= 0 || args[len(args)-1] != "stop" {
				t.Errorf("%s：%q", name, args)
			}
			continue
		}
		if i < 0 || !slices.Equal(args[i+1:], svcs) {
			t.Errorf("%s：服务名要跟在 -- 后面：%q", name, args)
		}
	}
}
