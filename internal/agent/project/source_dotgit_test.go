package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// 仓库里的符号链接指向 .git，再放一个同名的目录：不能顺着链接往检出目录的 .git 里写文件。
func TestCheckoutTreeNoWriteIntoDotGit(t *testing.T) {
	noDiskCheck(t)
	for _, target := range []string{".git", "./.git", ".git/hooks"} {
		r := newTestRepo(t)
		hooks := r.tree(object.TreeEntry{Name: "x", Mode: filemode.Executable, Hash: r.blob("#!/bin/sh\n")})
		inner := r.tree(object.TreeEntry{Name: "hooks", Mode: filemode.Dir, Hash: hooks})
		err := r.checkout(context.Background(), r.tree(
			object.TreeEntry{Name: "g", Mode: filemode.Symlink, Hash: r.blob(target)},
			object.TreeEntry{Name: "g", Mode: filemode.Dir, Hash: inner},
		))
		if err == nil {
			t.Errorf("链接 %s：顺着符号链接往下写应该拒绝", target)
		}
		for _, p := range []string{".git/hooks/x", ".git/hooks/hooks/x"} {
			if _, serr := os.Lstat(filepath.Join(r.dir, p)); serr == nil {
				t.Errorf("链接 %s：写进了 %s", target, p)
			}
		}
	}

	// 符号链接本身照常写出。
	r := newTestRepo(t)
	if err := r.checkout(context.Background(), r.tree(object.TreeEntry{Name: "g", Mode: filemode.Symlink, Hash: r.blob(".git")})); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(r.dir, "g")); err != nil || target != ".git" {
		t.Fatalf("符号链接：%q %v", target, err)
	}
}
