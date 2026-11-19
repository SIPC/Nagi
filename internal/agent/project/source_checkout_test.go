package project

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// testRepo 是一个只有对象、没有检出文件的仓库（和 NoCheckout 拉取下来的一样），dir 里只有 .git。
type testRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &testRepo{t: t, dir: dir, repo: repo}
}

func (r *testRepo) put(typ plumbing.ObjectType, encode func(plumbing.EncodedObject) error) plumbing.Hash {
	r.t.Helper()
	o := r.repo.Storer.NewEncodedObject()
	o.SetType(typ)
	if err := encode(o); err != nil {
		r.t.Fatal(err)
	}
	h, err := r.repo.Storer.SetEncodedObject(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *testRepo) blob(data string) plumbing.Hash {
	return r.put(plumbing.BlobObject, func(o plumbing.EncodedObject) error {
		w, err := o.Writer()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(data)); err != nil {
			return err
		}
		return w.Close()
	})
}

func (r *testRepo) tree(entries ...object.TreeEntry) plumbing.Hash {
	return r.put(plumbing.TreeObject, (&object.Tree{Entries: entries}).Encode)
}

func (r *testRepo) commit(tree plumbing.Hash) plumbing.Hash {
	sig := object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(0, 0)}
	return r.put(plumbing.CommitObject, (&object.Commit{Author: sig, Committer: sig, Message: "x", TreeHash: tree}).Encode)
}

func (r *testRepo) checkout(ctx context.Context, tree plumbing.Hash) error {
	return checkoutTree(ctx, r.repo, r.commit(tree), r.dir, r.dir)
}

// written 是 dir 里检出的文件个数和总大小（不算 .git）。
func (r *testRepo) written() (files int, size int64) {
	r.t.Helper()
	err := filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		size += info.Size()
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return files, size
}

func noDiskCheck(t *testing.T) {
	prev := checkDisk
	checkDisk = func(string, int64) error { return nil }
	t.Cleanup(func() { checkDisk = prev })
}

// 普通的仓库：文件、可执行文件、符号链接、多级目录照常写出，子模块留一个空目录。
func TestCheckoutTree(t *testing.T) {
	noDiskCheck(t)
	r := newTestRepo(t)
	hello := r.blob("hello\n")
	sub := r.tree(object.TreeEntry{Name: "x.conf", Mode: filemode.Regular, Hash: r.blob("x=1\n")})
	root := r.tree(
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: hello},
		object.TreeEntry{Name: "conf", Mode: filemode.Dir, Hash: r.tree(object.TreeEntry{Name: "d", Mode: filemode.Dir, Hash: sub})},
		object.TreeEntry{Name: "link", Mode: filemode.Symlink, Hash: r.blob("a.txt")},
		object.TreeEntry{Name: "mod", Mode: filemode.Submodule, Hash: plumbing.NewHash("1111111111111111111111111111111111111111")},
		object.TreeEntry{Name: "run.sh", Mode: filemode.Executable, Hash: r.blob("#!/bin/sh\n")},
	)
	if err := r.checkout(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(r.dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read("a.txt") != "hello\n" || read("conf/d/x.conf") != "x=1\n" {
		t.Fatal("文件内容不对")
	}
	if target, err := os.Readlink(filepath.Join(r.dir, "link")); err != nil || target != "a.txt" {
		t.Fatalf("符号链接：%q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(r.dir, "run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("可执行文件应保留执行权限：%v", err)
	}
	if info, err := os.Stat(filepath.Join(r.dir, "a.txt")); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("普通文件不应有执行权限：%v", err)
	}
	if info, err := os.Stat(filepath.Join(r.dir, "mod")); err != nil || !info.IsDir() {
		t.Fatalf("子模块应留一个空目录：%v", err)
	}
}

// 仓库很小、检出来很大：一个文件在目录树里用很多个名字引用。检出中磁盘不够了要停下并报磁盘不足，不能写完。
func TestCheckoutTreeLowDisk(t *testing.T) {
	prevCheck, prevBytes := checkDisk, gitDiskBytes
	t.Cleanup(func() { checkDisk, gitDiskBytes = prevCheck, prevBytes })
	gitDiskBytes = 1 << 20
	full := &metrics.LowDiskError{Total: 100 << 30, Free: 1 << 30}
	var calls atomic.Int32
	checkDisk = func(string, int64) error {
		if calls.Add(1) > 4 {
			return full
		}
		return nil
	}
	r := newTestRepo(t)
	big := r.blob(strings.Repeat("\x00", 1<<20))
	var entries []object.TreeEntry
	for i := range 64 {
		entries = append(entries, object.TreeEntry{Name: "f" + string(rune('A'+i/26)) + string(rune('a'+i%26)), Mode: filemode.Regular, Hash: big})
	}
	err := r.checkout(context.Background(), r.tree(entries...))
	if !errors.Is(err, full) {
		t.Fatalf("磁盘不够时应该停下并报磁盘不足：%v", err)
	}
	if _, size := r.written(); size > 8<<20 {
		t.Fatalf("磁盘不够以后还在写：写了 %d MiB", size>>20)
	}
}

// 目录树层层引用同一个子树，文件数按指数增长：超过上限就停下。
func TestCheckoutTreeTooManyFiles(t *testing.T) {
	noDiskCheck(t)
	prev := gitMaxFiles
	t.Cleanup(func() { gitMaxFiles = prev })
	gitMaxFiles = 100
	r := newTestRepo(t)
	level := func(mode filemode.FileMode, h plumbing.Hash) plumbing.Hash {
		var entries []object.TreeEntry
		for i := range 10 {
			entries = append(entries, object.TreeEntry{Name: string(rune('a' + i)), Mode: mode, Hash: h})
		}
		return r.tree(entries...)
	}
	tree := level(filemode.Regular, r.blob(""))
	for range 8 { // 10^9 个空文件
		tree = level(filemode.Dir, tree)
	}
	err := r.checkout(context.Background(), tree)
	if err == nil || !strings.Contains(err.Error(), "超过 100 个") {
		t.Fatalf("文件过多时应该停下：%v", err)
	}
	if files, _ := r.written(); files > 100 {
		t.Fatalf("超过上限以后还在写：%d 个", files)
	}
}

// 取消以后不再写。
func TestCheckoutTreeCanceled(t *testing.T) {
	noDiskCheck(t)
	r := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.checkout(ctx, r.tree(object.TreeEntry{Name: "a", Mode: filemode.Regular, Hash: r.blob("a")}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消以后应该停下：%v", err)
	}
	if files, _ := r.written(); files != 0 {
		t.Fatalf("取消以后还写了 %d 个文件", files)
	}
}

// 目录树是 git 服务器给的：不允许的名字、重名的条目、顺着符号链接往外写都要拒绝。
func TestCheckoutTreeRejectsEscape(t *testing.T) {
	noDiskCheck(t)
	for _, name := range []string{"..", ".", ".git", ".GIT", "a/b", ""} {
		r := newTestRepo(t)
		if err := r.checkout(context.Background(), r.tree(object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: r.blob("x")})); err == nil {
			t.Errorf("名字 %q 应该拒绝", name)
		}
		r = newTestRepo(t)
		inner := r.tree(object.TreeEntry{Name: "x", Mode: filemode.Regular, Hash: r.blob("x")})
		if err := r.checkout(context.Background(), r.tree(object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: inner})); err == nil {
			t.Errorf("目录名 %q 应该拒绝", name)
		}
	}

	// 先放一个指向外面的符号链接 a，再放一个同名的目录 a，里面有文件：不能写到外面。
	outside := t.TempDir()
	r := newTestRepo(t)
	inner := r.tree(object.TreeEntry{Name: "x", Mode: filemode.Regular, Hash: r.blob("x")})
	err := r.checkout(context.Background(), r.tree(
		object.TreeEntry{Name: "a", Mode: filemode.Symlink, Hash: r.blob(outside)},
		object.TreeEntry{Name: "a", Mode: filemode.Dir, Hash: inner},
	))
	if err == nil {
		t.Fatal("顺着符号链接往外写应该拒绝")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("写到了检出目录外面：%v", entries)
	}

	// 同名的符号链接和文件：不能顺着链接覆盖外面的文件。
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = newTestRepo(t)
	err = r.checkout(context.Background(), r.tree(
		object.TreeEntry{Name: "a", Mode: filemode.Symlink, Hash: r.blob(victim)},
		object.TreeEntry{Name: "a", Mode: filemode.Regular, Hash: r.blob("overwritten")},
	))
	if b, _ := os.ReadFile(victim); err == nil || string(b) != "keep" {
		t.Fatalf("顺着符号链接覆盖了外面的文件：%v %q", err, b)
	}
}

// 拉取中发现磁盘不够、clone 却照常返回时，也要报磁盘不足，不能接着往下写。
func TestGuardedCloneLowDiskAfterSuccess(t *testing.T) {
	prevCheck, prevEvery := checkDisk, gitDiskEvery
	t.Cleanup(func() { checkDisk, gitDiskEvery = prevCheck, prevEvery })
	gitDiskEvery = 10 * time.Millisecond
	full := &metrics.LowDiskError{Total: 100 << 30, Free: 1 << 30}
	var calls atomic.Int32
	checkDisk = func(string, int64) error {
		if calls.Add(1) > 1 {
			return full
		}
		return nil
	}
	err := guardedClone(context.Background(), t.TempDir(), func(ctx context.Context) error {
		<-ctx.Done() // 不看取消的那一段：照常做完，不报错
		return nil
	})
	if !errors.Is(err, full) {
		t.Fatalf("clone 没报错时也应该报磁盘不足：%v", err)
	}
}
