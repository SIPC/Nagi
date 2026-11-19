package fileops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// makeSpecial 在 p 建一个设备文件（kind 为 "device"，和 /dev/zero 一样是 c 1 5）或管道（"fifo"）。
// 没有 CAP_MKNOD 建不了设备文件时跳过测试。
func makeSpecial(t *testing.T, kind, p string) {
	t.Helper()
	var err error
	if kind == "device" {
		err = unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(1, 5)))
		if errors.Is(err, unix.EPERM) {
			t.Skip("没有 CAP_MKNOD，建不了设备文件")
		}
	} else {
		err = unix.Mkfifo(p, 0o666)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// within 在 d 之内运行 fn，超时就让测试失败（卡在打开管道上时 fn 不会返回）。
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("卡住了")
	}
}

// Lstat 时是普通文件，打开时已经换成设备文件或管道：拒绝，不卡住，也不打开设备。
func TestOpenRegularRejectsSwappedSpecial(t *testing.T) {
	for _, kind := range []string{"device", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			p := filepath.Join(dir, "f")
			if err := os.WriteFile(p, []byte("AAAA"), 0o644); err != nil {
				t.Fatal(err)
			}
			info, err := root.Lstat("f")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			makeSpecial(t, kind, p)
			within(t, 5*time.Second, func() {
				f, ok, err := OpenRegular(root, "f", info)
				if f != nil {
					f.Close()
				}
				if ok || err != nil {
					t.Errorf("应该当作已被修改：ok=%v err=%v", ok, err)
				}
			})
			// 拿新的 Lstat 结果（已经是特殊文件）也不打开。
			cur, err := root.Lstat("f")
			if err != nil {
				t.Fatal(err)
			}
			within(t, 5*time.Second, func() {
				if _, ok, _ := OpenRegular(root, "f", cur); ok {
					t.Error("特殊文件不应该打开")
				}
				if f, err := openNoDevice(root, "f"); !errors.Is(err, errNotRegular) {
					if f != nil {
						f.Close()
					}
					t.Errorf("openNoDevice 应该不打开特殊文件：%v", err)
				}
			})
		})
	}
}

// 目录被换成管道时，OpenDir 和经 RootFS 的遍历报错，不卡住。
func TestOpenDirRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	makeSpecial(t, "fifo", filepath.Join(dir, "d"))
	within(t, 5*time.Second, func() {
		if d, err := OpenDir(root, "d"); err == nil {
			d.Close()
			t.Error("管道不应该当作目录打开")
		}
		if _, err := fs.ReadDir(RootFS(root), "d"); err == nil {
			t.Error("RootFS 读管道应该报错")
		}
		if _, err := fs.Stat(RootFS(root), "d"); err != nil {
			t.Errorf("Stat 不打开文件，应该成功：%v", err)
		}
	})
}
