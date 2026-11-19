//go:build linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// v0.35.0-fix#6：现有目录被挂进容器（./:/app）时，容器把「快照包含的子目录」换成指向节点任意目录的链接，
// 快照不跟随，直接报错，卷外目录的内容不进快照。
func TestSnapshotRejectsSymlinkedSourceDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "identity.key"), secret)
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(app, "uploads")); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	dirs := []Dir{{Name: "project/uploads", Path: filepath.Join(app, "uploads"), Existing: true}}
	if _, err := r.Create(ctx, Manual, dirs, nil, nil); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("来源目录是符号链接时应该拒绝：%v", err)
	}
	// data/、volumes/ 被换成链接也一样（平时容器换不掉它们）。
	if err := os.Symlink(outside, filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: filepath.Join(root, "data")}}, nil, nil); err == nil {
		t.Fatal("来源目录是符号链接时应该拒绝")
	}
	// 指向上级目录里另一个目录的链接也不跟随。
	write(t, filepath.Join(app, "other", "x"), "x")
	if err := os.Symlink("other", filepath.Join(app, "inner")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openDirNoFollow(filepath.Join(app, "inner")); err == nil {
		t.Fatal("指向同级目录的链接也不应该跟随")
	}
}

// 容器不停交换子目录和指向卷外目录的链接：打开成功时，打开的一定是原来的子目录。
func TestSourceDirExchangeRace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "identity.key"), secret)
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, "uploads", "mine"), "mine")
	if err := os.Symlink(outside, filepath.Join(app, "link")); err != nil {
		t.Fatal(err)
	}
	dfd, err := unix.Open(app, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dfd)
	if err := unix.Renameat2(dfd, "uploads", dfd, "link", unix.RENAME_EXCHANGE); err != nil {
		t.Skipf("renameat2(RENAME_EXCHANGE) not supported here: %v", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Renameat2(dfd, "uploads", dfd, "link", unix.RENAME_EXCHANGE)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	opened := 0
	for range 2000 {
		src, closer, err := openSource(filepath.Join(app, "uploads"), "uploads")
		if err != nil {
			continue
		}
		opened++
		it, err := src.Iterate(context.Background())
		if err == nil {
			for {
				e, err := it.Next(context.Background())
				if err != nil || e == nil {
					break
				}
				if e.Name() == "identity.key" {
					t.Error("打开了链接指向的卷外目录")
				}
			}
			it.Close()
		}
		closer.Close()
	}
	if opened == 0 {
		t.Log("没有一次打开成功（交换太快），只验证了不跟随")
	}
}
