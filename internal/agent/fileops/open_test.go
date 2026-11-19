package fileops

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// OpenRegular 只打开 Lstat 时看到的那个文件：同一个文件变长了照常打开，换成了别的文件就拒绝。
func TestOpenRegularChecksSameFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := root.Lstat("f")
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = a.WriteString("more")
	a.Close()
	f, ok, err := OpenRegular(root, "f", info)
	if err != nil || !ok {
		t.Fatalf("同一个文件应该能打开：ok=%v err=%v", ok, err)
	}
	got, _ := io.ReadAll(io.LimitReader(f, info.Size()))
	f.Close()
	if string(got) != "one" {
		t.Errorf("读出 %q，应只读到 Lstat 时的大小", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "g"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "g"), p); err != nil {
		t.Fatal(err)
	}
	if f, ok, err := OpenRegular(root, "f", info); err != nil || ok {
		if f != nil {
			f.Close()
		}
		t.Errorf("换成别的文件以后应该拒绝：ok=%v err=%v", ok, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	dinfo, _ := root.Lstat("d")
	if _, ok, _ := OpenRegular(root, "d", dinfo); ok {
		t.Error("目录不应该当作普通文件打开")
	}
}
