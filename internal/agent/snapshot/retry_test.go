package snapshot

import (
	"context"
	"errors"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

// packBytes 是仓库里数据包的总大小和个数。
func packBytes(dir string) (int64, int) {
	var n int64
	count := 0
	// 文件系统存储按前缀分目录，数据包在 p 开头的目录里。
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, path)
		if err == nil && !d.IsDir() && strings.HasPrefix(filepath.ToSlash(rel), "p") {
			if info, err := d.Info(); err == nil {
				n += info.Size()
				count++
			}
		}
		return nil
	})
	return n, count
}

// 快照中途失败（超时、取消）以后再试，已经写好的内容要被复用，不能每次重新上传一遍：
// 内容不变时，几次失败加一次成功，仓库里的数据包大致只有一份数据那么大（见 docs/snapshot.md#失败与退避）。
// 失败时也不能留下快照记录。
func TestFailedSnapshotReusesWrittenContent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storage := filepath.Join(root, "backups")
	data := filepath.Join(root, "data")
	os.MkdirAll(data, 0o755)
	const size = 64 << 20
	b := make([]byte, size)
	rand.New(rand.NewSource(1)).Read(b)
	if err := os.WriteFile(filepath.Join(data, "blob.bin"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	open := func() *Repo {
		r, err := Open(ctx, "p1", storage, "test-password", filepath.Join(root, "state"))
		if err != nil {
			t.Fatal(err)
		}
		r.Limit = 10
		return r
	}

	failed, done := 0, 0
	for attempt := 1; attempt <= 3; attempt++ {
		r := open()
		_, before := packBytes(storage)
		actx, cancel := context.WithCancel(ctx)
		// 多写出一个数据包就取消，模拟暂停超时。
		go func() {
			for actx.Err() == nil {
				if _, n := packBytes(storage); n > before {
					cancel()
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		_, err := r.Create(actx, Manual, []Dir{{Name: "data", Path: data}}, nil, nil)
		cancel()
		if err == nil {
			// 前面失败时写好的内容被复用，剩下的很快就写完了，取消前就做完了。
			done++
		} else {
			failed++
		}
		list, lerr := r.List(ctx, Manual)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(list) != done {
			t.Fatalf("attempt %d (%v): %d snapshots, want %d: a failed attempt must not leave a snapshot", attempt, err, len(list), done)
		}
		r.Close(ctx)
		got, n := packBytes(storage)
		t.Logf("attempt %d: err=%v packs=%d bytes=%d", attempt, err, n, got)
	}

	if failed == 0 {
		t.Skip("no attempt was interrupted on this machine")
	}
	r := open()
	defer r.Close(ctx)
	if _, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: data}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, n := packBytes(storage)
	t.Logf("after success: packs=%d bytes=%d", n, got)
	// 不复用时每次失败会多出约 20 MB（一个数据包），三次失败加一次成功约 128 MB。
	if got > size*13/10 {
		t.Fatalf("repository grew to %d MB for %d MB of data: failed attempts were uploaded again", got>>20, size>>20)
	}
	list, err := r.List(ctx, Manual)
	if err != nil || len(list) != done+1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	if err := r.Restore(ctx, list[0].ID, "data", filepath.Join(root, "restore")); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(root, "restore", "blob.bin"))
	if err != nil || len(restored) != size || string(restored[:1024]) != string(b[:1024]) || string(restored[size-1024:]) != string(b[size-1024:]) {
		t.Fatalf("restored file does not match (%d bytes, %v)", len(restored), err)
	}
}

// 立即清理（节点设置里的「清理快照仓库」）：旧版本失败时留下、没有写进索引的数据包要删掉，已有的快照照样完好、能恢复。
func TestReclaimRemovesOrphanPacks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storage := filepath.Join(root, "backups")
	data := filepath.Join(root, "data")
	os.MkdirAll(data, 0o755)
	if err := os.WriteFile(filepath.Join(data, "a.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, "p1", storage, "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if _, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: data}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	kept, _ := packBytes(storage)

	// 像旧版本那样：写了 64 MB 以后失败，没有提交索引。
	junk := make([]byte, 64<<20)
	rand.New(rand.NewSource(2)).Read(junk)
	_ = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		ow := w.NewObjectWriter(ctx, object.WriterOptions{})
		ow.Write(junk)
		ow.Close()
		return errors.New("simulated failure")
	})
	before, _ := packBytes(storage)
	if before < kept+(32<<20) {
		t.Fatalf("the failed write left only %d bytes of packs", before-kept)
	}

	time.Sleep(1100 * time.Millisecond) // kopia 的时间戳精确到秒
	if err := r.Maintain(ctx, true); err != nil {
		t.Fatal(err)
	}
	after, _ := packBytes(storage)
	t.Logf("packs: kept %d, with orphans %d, after reclaim %d", kept, before, after)
	if after > kept+(1<<20) {
		t.Fatalf("orphan packs were not reclaimed: %d bytes left, %d expected", after, kept)
	}
	if err := r.Verify(ctx); err != nil {
		t.Fatalf("verify after reclaim: %v", err)
	}
	list, err := r.List(ctx, Manual)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	if err := r.Restore(ctx, list[0].ID, "data", filepath.Join(root, "restore")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "restore", "a.txt")); err != nil || string(b) != "keep me" {
		t.Fatalf("restored %q %v", b, err)
	}
}
