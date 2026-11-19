package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// fanout 造一棵目录树：depth 层，每层 n 个条目都引用同一个下一层；最底层是 leaf（文件或目录）。
func (r *testRepo) fanout(n, depth int, leafMode filemode.FileMode, leaf plumbing.Hash) plumbing.Hash {
	h, mode := leaf, leafMode
	for range depth {
		var entries []object.TreeEntry
		for i := range n {
			entries = append(entries, object.TreeEntry{Name: fmt.Sprintf("e%02d", i), Mode: mode, Hash: h})
		}
		h, mode = r.tree(entries...), filemode.Dir
	}
	return h
}

// entriesIn 数 dir 下除 .git 以外的文件和目录个数（每个占一个 inode）。
func entriesIn(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if p != dir {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// inodeLimit 模拟一块 inode 只剩 budget 个的盘：检查磁盘时数一下 dir 里已经建了多少个条目，超过 budget 就报 inode 不足。
func inodeLimit(t *testing.T, dir string, budget int) *int {
	t.Helper()
	calls := new(int)
	prev := checkDisk
	checkDisk = func(string, int64) error {
		*calls++
		if n := entriesIn(t, dir); n > budget {
			return &metrics.LowDiskError{Total: 100 << 30, Free: 50 << 30, Inodes: 1 << 20, FreeInodes: 0}
		}
		return nil
	}
	t.Cleanup(func() { checkDisk = prev })
	return calls
}

// 大量空文件不占空间、只占 inode：按条目数检查磁盘，inode 不够时停下，不能一直写到 inode 用完。
func TestCheckoutTreeStopsOnInodes(t *testing.T) {
	r := newTestRepo(t)
	const budget = 3000
	calls := inodeLimit(t, r.dir, budget)
	tree := r.fanout(16, 4, filemode.Regular, r.blob("")) // 65536 个空文件，加 4368 个目录
	err := r.checkout(context.Background(), tree)
	var low *metrics.LowDiskError
	if !errors.As(err, &low) || low.Inodes == 0 {
		t.Fatalf("inode 不够时应该停下并报 inode 不足：%v（检查了 %d 次）", err, *calls)
	}
	if n := entriesIn(t, r.dir); n > budget+gitDiskEntries {
		t.Fatalf("inode 不够以后还在建：建了 %d 个", n)
	}
}

// 每个空文件放在一条很深的独有目录链里：目录也算条目，路径深度也有上限。
func TestCheckoutTreeDeepDirs(t *testing.T) {
	noDiskCheck(t)
	r := newTestRepo(t)
	// 2 个文件，各在一条 300 层的目录链里（共享子树，仓库只有几百个对象）。
	tree := r.fanout(2, 300, filemode.Regular, r.blob(""))
	err := r.checkout(context.Background(), tree)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("超过 %d 层", gitMaxDepth)) {
		t.Fatalf("路径太深时应该拒绝：%v", err)
	}
	if n := entriesIn(t, r.dir); n > gitMaxDepth {
		t.Fatalf("拒绝之前已经建了 %d 个目录", n)
	}
}

// 只有目录的目录树按指数增长（2^40 个目录），里面没有文件：目录也计入条目上限，很快停下，不会空转到超时。
func TestCheckoutTreeDirsOnly(t *testing.T) {
	noDiskCheck(t)
	prev := gitMaxEntries
	t.Cleanup(func() { gitMaxEntries = prev })
	gitMaxEntries = 10_000
	r := newTestRepo(t)
	tree := r.fanout(2, 40, filemode.Dir, r.tree())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := r.checkout(ctx, tree)
	if err == nil || !strings.Contains(err.Error(), "超过 10000 个") {
		t.Fatalf("只有目录的目录树也应该按条目上限停下：%v", err)
	}
}

// 复制到 data/ 时也检查磁盘：按条目数（空文件）和字节数。
func TestSyncDirChecksDisk(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for i := range 3 * gitDiskEntries {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := inodeLimit(t, dst, 1500)
	err := syncDir(src, dst, nil, func(string) {})
	var low *metrics.LowDiskError
	if !errors.As(err, &low) {
		t.Fatalf("复制时 inode 不够应该停下：%v（检查了 %d 次）", err, *calls)
	}
	if n := entriesIn(t, dst); n > 1500+gitDiskEntries {
		t.Fatalf("inode 不够以后还在复制：%d 个", n)
	}

	// 按字节：一个 4 MiB 的文件，每 1 MiB 检查一次，第二次检查时磁盘不够。
	prevBytes := gitDiskBytes
	t.Cleanup(func() { gitDiskBytes = prevBytes })
	gitDiskBytes = 1 << 20
	src, dst = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	full := &metrics.LowDiskError{Total: 100 << 30, Free: 1 << 30}
	n := 0
	checkDisk = func(string, int64) error {
		if n++; n > 1 {
			return full
		}
		return nil
	}
	if err := syncDir(src, dst, nil, func(string) {}); !errors.Is(err, full) {
		t.Fatalf("复制时磁盘不够应该停下：%v", err)
	}
	if info, err := os.Stat(filepath.Join(dst, "big")); err == nil && info.Size() > 3<<20 {
		t.Fatalf("磁盘不够以后还在写：%d", info.Size())
	}
}

// 检出的文件放不进 data/ 时，复制之前就停下，data/ 里的旧文件不动。
func TestInstallCheckoutChecksSpace(t *testing.T) {
	p := containerPaths{root: t.TempDir()}
	write(t, p.dataDir(), "old.txt", "old")
	write(t, p.checkoutDir(), "new.txt", "new")
	full := &metrics.LowDiskError{Total: 100 << 30, Free: 3 << 30}
	prev := checkDisk
	t.Cleanup(func() { checkDisk = prev })
	var need int64
	checkDisk = func(_ string, n int64) error {
		if n > 0 {
			need = n
			return full
		}
		return nil
	}
	if err := installCheckout(p, 2<<30, nil, func(string) {}); !errors.Is(err, full) {
		t.Fatalf("放不下时应该报磁盘不足：%v", err)
	}
	if need != 2<<30 {
		t.Fatalf("应该按检出的大小检查：%d", need)
	}
	if s, ok := read(t, p.dataDir(), "old.txt"); !ok || s != "old" {
		t.Fatal("复制之前就该停下，data/ 里的旧文件不能删")
	}
	if _, ok := read(t, p.dataDir(), "new.txt"); ok {
		t.Fatal("放不下时不应该复制")
	}
}

// 在真实的小文件系统上验证（例如 docker run --tmpfs /small:nr_inodes=20000）：NAGI_TEST_SMALL_FS 指向它的挂载点。
// 检出大量空文件时，按 inode 停下并报 inode 不足，而不是写到 ENOSPC。
func TestCheckoutTreeRealInodeLimit(t *testing.T) {
	base := os.Getenv("NAGI_TEST_SMALL_FS")
	if base == "" {
		t.Skip("没有设置 NAGI_TEST_SMALL_FS")
	}
	if inodes, _ := metrics.DiskInodes(base); inodes == 0 {
		t.Skip("读不到 inode 个数")
	}
	dir, err := os.MkdirTemp(base, "checkout-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := newTestRepo(t) // 对象放在普通的临时目录里，只把检出写到小文件系统上
	tree := r.fanout(16, 4, filemode.Regular, r.blob(""))
	_, err = checkoutTree(context.Background(), r.repo, r.commit(tree), dir, dir)
	var low *metrics.LowDiskError
	if !errors.As(err, &low) || low.Inodes == 0 {
		t.Fatalf("应该在 inode 用完之前停下并报 inode 不足：%v", err)
	}
	_, free := metrics.DiskInodes(base)
	t.Logf("停下时剩余 inode %d 个，共 %d 个；建了 %d 个条目", free, low.Inodes, entriesIn(t, dir))
	if free == 0 {
		t.Fatal("inode 已经用完")
	}
}
