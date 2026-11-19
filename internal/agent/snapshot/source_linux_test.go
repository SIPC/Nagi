//go:build linux

package snapshot

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"golang.org/x/sys/unix"
)

const secret = "NODE-PRIVATE-KEY-must-not-leave-the-node"

// sourceChild 打开来源目录，返回 name 这一项（这时 Lstat 看到的样子）。
func sourceChild(t *testing.T, dir, name string) fs.Entry {
	t.Helper()
	src, closer, err := openSource(dir, "data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	e, err := src.Child(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// 遍历时是普通文件，打开前被换成指向卷外的符号链接（v0.35.0#1），或者卷里的另一个文件：打开失败，读不到别的文件。
func TestSourceOpenRejectsSwappedFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, outside, secret)
	dir := filepath.Join(root, "data")
	write(t, filepath.Join(dir, "f"), "ordinary")
	write(t, filepath.Join(dir, "other"), "other file in the volume")

	for _, target := range []string{outside, "other"} {
		e := sourceChild(t, dir, "f")
		f, ok := e.(fs.File)
		if !ok {
			t.Fatalf("f is %T", e)
		}
		os.Remove(filepath.Join(dir, "f"))
		if err := os.Symlink(target, filepath.Join(dir, "f")); err != nil {
			t.Fatal(err)
		}
		r, err := f.Open(ctx)
		if err == nil {
			b, _ := io.ReadAll(r)
			r.Close()
			t.Fatalf("swapped to symlink -> %s: opened and read %q", target, b)
		}
		os.Remove(filepath.Join(dir, "f"))
		write(t, filepath.Join(dir, "f"), "ordinary")
	}

	// 换成另一个普通文件（不同的 inode）也不行。
	e := sourceChild(t, dir, "f")
	os.Rename(filepath.Join(dir, "other"), filepath.Join(dir, "f"))
	if r, err := e.(fs.File).Open(ctx); err == nil {
		r.Close()
		t.Fatal("replaced by a different file: should not open")
	} else if !errors.Is(err, errChanged) {
		t.Logf("err = %v", err)
	}
}

// 目录在遍历以后被换成指向卷外目录的符号链接：不能读出卷外目录的内容。
func TestSourceIterateRejectsSwappedDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "outside", "key"), secret)
	dir := filepath.Join(root, "data")
	write(t, filepath.Join(dir, "d", "x"), "x")

	e := sourceChild(t, dir, "d")
	d, ok := e.(fs.Directory)
	if !ok {
		t.Fatalf("d is %T", e)
	}
	os.RemoveAll(filepath.Join(dir, "d"))
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	if entries, err := fs.GetAllEntries(ctx, d); err == nil {
		t.Fatalf("swapped directory should not be read, got %d entries", len(entries))
	}
}

// 遍历时是普通文件，打开前被换成 FIFO（v0.35.0#7）：打开不阻塞，直接失败。
func TestSourceOpenFIFODoesNotBlock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "f"), "ordinary")
	e := sourceChild(t, dir, "f")
	os.Remove(filepath.Join(dir, "f"))
	if err := syscall.Mkfifo(filepath.Join(dir, "f"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		r, err := e.(fs.File).Open(ctx)
		if err == nil {
			r.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO should not be opened as a file")
		}
	case <-time.After(5 * time.Second):
		// 解开卡住的 open(2)，免得测试进程挂住。
		if w, err := os.OpenFile(filepath.Join(dir, "f"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		t.Fatal("opening a FIFO blocked")
	}

	// 整份快照也不会卡住：遍历时就是 FIFO 的跳过，不进快照。
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	write(t, filepath.Join(dir, "g"), "g")
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	id, err := r.Create(cctx, Manual, []Dir{{Name: "data", Path: dir}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := r.ListDir(ctx, id, "data")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "g" {
		t.Fatalf("snapshot = %+v, want only g", list)
	}
}

// 符号链接按链接本身存，不跟随：快照里是链接，恢复出来也是链接，卷外文件的内容不进快照。
func TestSourceStoresSymlinksAsLinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, outside, secret)
	data := filepath.Join(root, "data")
	write(t, filepath.Join(data, "f"), "f")
	os.Symlink(outside, filepath.Join(data, "link"))
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	id, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: data}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.Stat(ctx, id, "data/link")
	if err != nil || !info.IsSymlink {
		t.Fatalf("link in snapshot = %+v %v", info, err)
	}
	target := filepath.Join(root, "restore")
	if err := r.Restore(ctx, id, "data", target); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(target, "link")); err != nil || got != outside {
		t.Fatalf("restored link = %q %v", got, err)
	}
}

// v0.35.0#1 的利用方式：容器用 renameat2(RENAME_EXCHANGE) 不停交换普通文件 f 和指向卷外文件的符号链接 g，
// 同时改修改时间，让 kopia 不复用上一份的内容。不管哪次快照、哪个名字，快照里都不能出现卷外文件的内容。
func TestSnapshotExchangeRaceDoesNotLeakOutsideFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := filepath.Join(root, "outside", "identity.key")
	write(t, outside, secret)
	data := filepath.Join(root, "data")
	write(t, filepath.Join(data, "f"), strings.Repeat("A", len(secret)))
	// 子目录也换：d 和指向卷外目录的链接 e。
	write(t, filepath.Join(data, "d", "x"), "x")
	if err := os.Symlink(outside, filepath.Join(data, "g")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(data, "e")); err != nil {
		t.Fatal(err)
	}
	dfd, err := unix.Open(data, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dfd)
	if err := unix.Renameat2(dfd, "f", dfd, "g", unix.RENAME_EXCHANGE); err != nil {
		t.Skipf("renameat2(RENAME_EXCHANGE) not supported here: %v", err)
	}

	stop := make(chan struct{})
	var swaps atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts := []unix.Timespec{unix.NsecToTimespec(0), unix.NsecToTimespec(0)}
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Renameat2(dfd, "f", dfd, "g", unix.RENAME_EXCHANGE)
			_ = unix.Renameat2(dfd, "d", dfd, "e", unix.RENAME_EXCHANGE)
			ts[1] = unix.NsecToTimespec(time.Now().UnixNano() + i)
			for _, n := range []string{"f", "g"} {
				_ = unix.UtimesNanoAt(dfd, n, ts, unix.AT_SYMLINK_NOFOLLOW)
			}
			swaps.Add(1)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	regular := 0
	for i := 0; i < 40; i++ {
		id, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: data}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = r.WalkFiles(ctx, id, "data", func(sub string, info FileInfo, open func() (io.ReadCloser, error)) error {
			if open == nil {
				return nil
			}
			rc, err := open()
			if err != nil {
				return err
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if strings.Contains(string(b), secret) {
				t.Errorf("snapshot %d: %s contains the outside file", i, sub)
			}
			if sub == "f" || sub == "g" {
				regular++
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d swaps, ordinary file captured %d times in 40 snapshots", swaps.Load(), regular)
}
