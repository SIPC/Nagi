package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 容器不停把目录和管道互换时，removeSymlinks 和 syncRoot 遍历到「目录」再去读它，可能读到的是管道：
// 要报错或跳过，不能卡在打开管道上。
func TestSafeFSWalkDoesNotHangOnFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeSpecialFile(t, "fifo", filepath.Join(dir, "p"))
	swapFiles(t, filepath.Join(dir, "d"), filepath.Join(dir, "p"))
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 300 {
			_ = removeSymlinks(dir)
			// syncRoot 的清理也经 walkRoot；直接调用它会删掉管道，换不下去了。
			_ = walkRoot(root, func(rel string, d os.DirEntry, err error) error { return err })
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("遍历卡在了换成管道的目录上")
	}
}
