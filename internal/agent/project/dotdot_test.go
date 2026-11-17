package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/db"
)

// N2：项目根目录下名字以 .. 开头的目录（`..x`）在里面，within 不能把它当成在外面；否则 linkBelow 跳过符号链接的检查，
// 操作员能经过容器里建的符号链接把容器目录放到项目根目录以外。
func TestWithinDotDotName(t *testing.T) {
	for _, c := range []struct {
		child, parent string
		want          bool
	}{
		{"/srv/nagi/..x", "/srv/nagi", true},
		{"/srv/nagi/..x/data", "/srv/nagi", true},
		{"/srv/nagi/...", "/srv/nagi", true},
		{"/srv/nagi", "/srv/nagi", true},
		{"/srv", "/srv/nagi", false},
		{"/srv/other", "/srv/nagi", false},
		{"/srv/nagix", "/srv/nagi", false},
	} {
		if got := within(filepath.FromSlash(c.child), filepath.FromSlash(c.parent)); got != c.want {
			t.Errorf("within(%q, %q) = %v，应为 %v", c.child, c.parent, got, c.want)
		}
	}
}

// 审查里的利用路径：<项目根>/..x/data/ln 是指向 / 的符号链接，经过它的容器目录必须被拒绝；
// 名字以 .. 开头的目录本身也不收。
func TestCheckDirDotDotLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CheckDir 只接受 Unix 的绝对路径")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "..x", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "..x", "data", "ln")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	if !linkBelow(root, filepath.Join(root, "..x", "data", "ln", "crontabs")) {
		t.Fatal("经过 ..x 下的符号链接应该被发现")
	}
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
	m.ProjectsRoot = func() string { return root }
	for _, dir := range []string{
		filepath.Join(root, "..x", "data", "ln", "crontabs"),
		filepath.Join(root, "..x"),
		filepath.Join(root, "app", "..cache"),
	} {
		if _, err := m.CheckDir(ctx, dir); err == nil {
			t.Errorf("CheckDir(%q) 应该拒绝", dir)
		}
	}
	if _, err := m.CheckDir(ctx, filepath.Join(root, "web", "app")); err != nil {
		t.Errorf("普通目录应该接受：%v", err)
	}
}
