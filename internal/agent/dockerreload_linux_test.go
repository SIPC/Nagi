package agent

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/SIPC/nagi/internal/agent/docker"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// TestSetMirrorsReloadsDocker 在真的 dockerd 上检查：保存加速地址后 Docker 重新加载配置、用上新地址，dockerd 没有重启。
// 会改 /etc/docker/daemon.json、向 dockerd 发 SIGHUP，只在一次性的 docker:dind 容器里运行，不要在开发机上跑：
//
//	GOOS=linux CGO_ENABLED=0 go test -c -o agent.test ./internal/agent
//	docker run -d --privileged --name nagi-dind docker:dind
//	docker cp agent.test nagi-dind:/agent.test
//	docker exec -e NAGI_TEST_DOCKER_RELOAD=1 nagi-dind /agent.test -test.run TestSetMirrorsReloadsDocker -test.v
//	docker rm -f -v nagi-dind
func TestSetMirrorsReloadsDocker(t *testing.T) {
	if os.Getenv("NAGI_TEST_DOCKER_RELOAD") != "1" {
		t.Skip("只在一次性的 docker:dind 容器里运行：NAGI_TEST_DOCKER_RELOAD=1")
	}
	dc, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	s := &agentService{node: &agent{docker: dc, log: slog.Default()}}
	ctx := context.Background()
	pid, _ := hostDockerReloader.pidFile()

	want := []string{"https://mirror-a.example.com", "https://mirror-b.example.com/"}
	res, err := s.SetMirrors(ctx, &nagiv1.Mirrors{Mirrors: want})
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetReloaded() || res.GetReloadError() != "" || !sameMirrors(res.GetActive(), want) {
		t.Fatalf("应重新加载并用上新地址：%+v", res)
	}
	if active, _ := dc.Mirrors(ctx); !sameMirrors(active, want) {
		t.Fatalf("docker info 里的加速地址：%v", active)
	}

	res, err = s.SetMirrors(ctx, &nagiv1.Mirrors{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetReloaded() || len(res.GetActive()) != 0 || slices.Contains(res.GetMirrors(), want[0]) {
		t.Fatalf("清空后应不再使用加速地址：%+v", res)
	}
	if now, _ := hostDockerReloader.pidFile(); now != pid {
		t.Fatalf("dockerd 不应重启：pid %d → %d", pid, now)
	}
}
