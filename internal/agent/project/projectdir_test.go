package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 现有目录不能在 Nagi 的数据目录里，也不能和别的容器的目录或现有目录重叠。
func TestCheckProjectDir(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), nodestore.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m, err := NewManager(d, nil, DefaultSubnetPool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.ReservedDirs = []string{"/var/lib/nagi"}
	other := &nagiv1.ContainerSpec{ProjectId: "p2", Name: "db", Dir: "/srv/nagi/p2/db", Config: &nagiv1.ContainerConfig{}}
	shared := &nagiv1.ContainerSpec{ProjectId: "p3", Name: "app", Dir: "/srv/nagi/p3/app", Config: &nagiv1.ContainerConfig{ProjectDir: "/opt/immich"}}
	for _, s := range []*nagiv1.ContainerSpec{other, shared} {
		if err := m.store(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(dir string) *nagiv1.ContainerSpec {
		return &nagiv1.ContainerSpec{ProjectId: "p1", Name: "web", Config: &nagiv1.ContainerConfig{ProjectDir: dir}}
	}
	for dir, ok := range map[string]bool{
		"":                     true,
		"/opt/gitea":           true,
		"/var/lib/nagi":        false,
		"/var/lib/nagi/x":      false,
		"/var/lib":             false,
		"/srv/nagi/p2/db/data": false,
		"/srv/nagi/p2":         false,
		"/opt/immich":          false,
		"/opt/immich/library":  false,
	} {
		if err := m.checkProjectDir(ctx, mk(dir)); (err == nil) != ok {
			t.Errorf("%q: err = %v", dir, err)
		}
	}
	// 自己原来的现有目录不算重叠。
	if err := m.checkProjectDir(ctx, shared); err != nil {
		t.Errorf("own dir: %v", err)
	}
}

// 项目根目录以下经过符号链接的容器目录要拒绝；根目录本身是链接不算。
func TestLinkBelow(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "disk")
	if err := os.MkdirAll(filepath.Join(real, "p", "old", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "srv")
	if err := os.Symlink(real, root); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	outside := filepath.Join(base, "etc")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "p", "old", "data", "link")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		filepath.Join(root, "p", "new"):                      false,
		filepath.Join(root, "p", "old", "data", "x"):         false,
		filepath.Join(root, "p", "old", "data", "link", "x"): true,
		filepath.Join(root, "p", "old", "data", "link"):      true,
		filepath.Join(base, "elsewhere", "link", "x"):        false,
	} {
		if got := linkBelow(root, dir); got != want {
			t.Errorf("%s: linkBelow = %v, want %v", dir, got, want)
		}
	}
}
