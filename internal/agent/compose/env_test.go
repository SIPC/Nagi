package compose

import (
	"slices"
	"strings"
	"testing"
)

// docker 命令只拿到它自己要用的环境变量，节点进程里别的值不能被 compose 的 ${变量} 读到。
func TestBaseEnv(t *testing.T) {
	t.Setenv("NAGI_TEST_SECRET", "sentinel")
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("LC_ALL", "C.UTF-8")
	env := baseEnv()
	for _, kv := range env {
		if strings.Contains(kv, "sentinel") {
			t.Fatalf("传了 %s", kv)
		}
	}
	for _, want := range []string{"DOCKER_HOST=unix:///run/user/1000/docker.sock", "LC_ALL=C.UTF-8"} {
		if !slices.Contains(env, want) {
			t.Errorf("缺少 %s", want)
		}
	}
}
