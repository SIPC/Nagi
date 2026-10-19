package project

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestDangerousSettings(t *testing.T) {
	base := t.TempDir()
	load := func(compose string) []string {
		t.Helper()
		cp, err := loadContainer(context.Background(), &nagiv1.ContainerConfig{Compose: compose}, "app", nil, filepath.Join(base, "data"), "test")
		if err != nil {
			t.Fatal(err)
		}
		return dangerousSettings(cp, base)
	}
	safe := load(`services:
  web:
    image: nginx
    network_mode: service:db
    volumes: [./uploads:/u, pg:/var/lib/pg]
  db:
    image: postgres
volumes:
  pg: {}
`)
	if len(safe) != 0 {
		t.Fatalf("普通配置被当成危险的：%v", safe)
	}
	got := strings.Join(load(`services:
  x:
    image: alpine
    privileged: true
    pid: host
    cap_add: [SYS_ADMIN]
    volumes: [/var/run/docker.sock:/s, ../../other:/o, ext:/e]
volumes:
  ext:
    external: true
`), "\n")
	for _, want := range []string{"privileged", "pid: host", "SYS_ADMIN", "docker.sock", "other", "ext"} {
		if !strings.Contains(got, want) {
			t.Errorf("没有发现 %s：\n%s", want, got)
		}
	}
}
