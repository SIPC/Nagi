package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
)

// 自己实现的来源给出的条目（名字、类型、大小、修改时间、权限、属主）要和 kopia 的 localfs 一致：
// 升级以前用 localfs 拍的快照，升级以后没变的文件照样直接复用，不用重新读一遍。
func TestSourceMatchesLocalfs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "hello")
	write(t, filepath.Join(dir, "sub", "b.bin"), "0123456789")
	os.MkdirAll(filepath.Join(dir, "empty"), 0o700)
	os.Chtimes(filepath.Join(dir, "a.txt"), time.Now(), time.Unix(1700000000, 123456789))
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Logf("no symlink on this machine: %v", err)
	}

	var compare func(rel string, ours, theirs fs.Directory)
	compare = func(rel string, ours, theirs fs.Directory) {
		a, err := fs.GetAllEntries(ctx, ours)
		if err != nil {
			t.Fatal(err)
		}
		b, err := fs.GetAllEntries(ctx, theirs)
		if err != nil {
			t.Fatal(err)
		}
		fs.Sort(a)
		fs.Sort(b)
		if len(a) != len(b) {
			t.Fatalf("%s: %d entries, localfs has %d", rel, len(a), len(b))
		}
		for i := range a {
			x, y := a[i], b[i]
			if x.Name() != y.Name() || x.Mode() != y.Mode() || x.Size() != y.Size() || !x.ModTime().Equal(y.ModTime()) || x.Owner() != y.Owner() {
				t.Errorf("%s/%s: ours %v %d %v %v, localfs %v %d %v %v", rel, x.Name(), x.Mode(), x.Size(), x.ModTime(), x.Owner(),
					y.Mode(), y.Size(), y.ModTime(), y.Owner())
			}
			_, xd := x.(fs.Directory)
			_, yd := y.(fs.Directory)
			_, xf := x.(fs.File)
			_, yf := y.(fs.File)
			_, xs := x.(fs.Symlink)
			_, ys := y.(fs.Symlink)
			if xd != yd || xf != yf || xs != ys {
				t.Errorf("%s/%s: type %T, localfs %T", rel, x.Name(), x, y)
			}
			if xd && yd {
				compare(rel+"/"+x.Name(), x.(fs.Directory), y.(fs.Directory))
			}
		}
	}
	ours, closer, err := openSource(dir, "data")
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	theirs, err := localfs.Directory(dir)
	if err != nil {
		t.Fatal(err)
	}
	compare("data", ours, theirs)

	// 读出的内容一致。
	e, err := ours.Child(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.(fs.File).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
}

// 增量复用照旧：大小、修改时间、权限都没变的文件直接用上一份快照里的内容，不重新读。
// 这里故意改了内容但保留大小和修改时间，复用时第二份快照里还是旧内容。
func TestSourceReusesUnchangedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	data := filepath.Join(root, "data")
	f := filepath.Join(data, "sub", "f.txt")
	write(t, f, "aaaa")
	mtime := time.Unix(1700000000, 0)
	os.Chtimes(f, mtime, mtime)
	dirs := []Dir{{Name: "data", Path: data}}
	if _, err := r.Create(ctx, Manual, dirs, nil, nil); err != nil {
		t.Fatal(err)
	}
	write(t, f, "bbbb")
	os.Chtimes(f, mtime, mtime)
	id, err := r.Create(ctx, Manual, dirs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSnapshotFile(t, r, id, "data/sub/f.txt"); got != "aaaa" {
		t.Fatalf("unchanged metadata should reuse the previous content, got %q", got)
	}
	// 修改时间变了就重新读。
	os.Chtimes(f, mtime, mtime.Add(time.Second))
	id, err = r.Create(ctx, Manual, dirs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSnapshotFile(t, r, id, "data/sub/f.txt"); got != "bbbb" {
		t.Fatalf("changed file should be read again, got %q", got)
	}
}

// readSnapshotFile 读出快照里的一个普通文件。
func readSnapshotFile(t *testing.T, r *Repo, id, name string) string {
	t.Helper()
	var out string
	found := false
	err := r.WalkFiles(context.Background(), id, name, func(sub string, _ FileInfo, open func() (io.ReadCloser, error)) error {
		if sub != "" || open == nil {
			return nil
		}
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		out, found = string(b), true
		return err
	})
	if err != nil || !found {
		t.Fatalf("read %s from snapshot: found=%v err=%v", name, found, err)
	}
	return out
}
