package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// 目录树引用了一个仓库里没有的子树（git 服务器少发了对象）：要报错，不能当作检出完了，
// 否则后面的文件都没写、照样部署，data/ 里的旧文件还会被删掉。
func TestCheckoutTreeMissingSubtree(t *testing.T) {
	noDiskCheck(t)
	r := newTestRepo(t)
	missing := plumbing.NewHash("2222222222222222222222222222222222222222")
	tree := r.tree(
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: r.blob("a")},
		object.TreeEntry{Name: "conf", Mode: filemode.Dir, Hash: missing},
		object.TreeEntry{Name: "z.txt", Mode: filemode.Regular, Hash: r.blob("z")},
	)
	err := r.checkout(context.Background(), tree)
	if err == nil || !strings.Contains(err.Error(), `"conf"`) {
		t.Fatalf("子树缺失时应该报错：%v", err)
	}
	if _, serr := os.Stat(filepath.Join(r.dir, "z.txt")); serr == nil {
		t.Fatal("报错以后不应该接着写")
	}

	// 深一层的子树缺失也一样。
	r = newTestRepo(t)
	tree = r.tree(object.TreeEntry{Name: "app", Mode: filemode.Dir, Hash: r.tree(
		object.TreeEntry{Name: "lib", Mode: filemode.Dir, Hash: missing},
	)})
	if err := r.checkout(context.Background(), tree); err == nil || !strings.Contains(err.Error(), `"app/lib"`) {
		t.Fatalf("深一层的子树缺失时应该报错：%v", err)
	}
}
