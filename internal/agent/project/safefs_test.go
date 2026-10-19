package project

import (
	"os"
	"path/filepath"
	"testing"
)

// 容器把 data/ 里的路径换成指向外面的符号链接，同步文件时不能写到外面去。
func TestSyncDirDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	for _, d := range []string{outside, filepath.Join(src, "conf"), dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "conf", "evil"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "conf")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	// 保留路径：清理时不会删掉它，第二步复制时 conf 已经存在，以现有内容为准。
	// 不保留：清理时删掉链接本身，再按 src 建真目录。两种情况都不能写到 outside。
	for _, preserve := range [][]string{nil, {"conf/evil"}} {
		_ = syncDir(src, dst, preserve, func(string) {})
		if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
			t.Fatalf("保留路径 %v：文件写到了链接指向的目录", preserve)
		}
		os.RemoveAll(filepath.Join(dst, "conf"))
		os.Symlink(outside, filepath.Join(dst, "conf"))
	}
}

func TestNoSymlinkBelow(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "data", "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "data", "link")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	if err := noSymlinkBelow(base, filepath.Join(base, "data", "real", "x")); err != nil {
		t.Fatalf("普通目录被拒绝：%v", err)
	}
	if err := noSymlinkBelow(base, filepath.Join(base, "data", "link", "x")); err == nil {
		t.Fatal("经过符号链接的路径没有被拒绝")
	}
	if err := noSymlinkBelow(base, "/etc"); err != nil {
		t.Fatalf("不在容器目录下的路径不检查：%v", err)
	}
}
