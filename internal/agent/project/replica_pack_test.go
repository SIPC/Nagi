package project

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/agent/snapshot"
)

// 数据包按第一级目录认：kopia 把文件名切成几级目录，文件名本身不以 p、q 开头。
func TestIsPack(t *testing.T) {
	cases := map[string]bool{
		"p/147/fcb5c5bf5eb986d4c34263d4e91f7-sc9f.f": true,
		"q/14d/cd1189a41b29cc6649857ca6f3f8c-sc9f.f": true,
		"p0123abcdef.f":      true, // 没有分级目录
		"x/n0_/94eaaebf35.f": false,
		"_log/abc.f":         false,
		"kopia.repository.f": false,
		".shards":            false,
	}
	for rel, want := range cases {
		if got := isPack(rel); got != want {
			t.Errorf("isPack(%q) = %v，应为 %v", rel, got, want)
		}
	}
}

// 真实的 kopia 仓库：数据包都认得出来，索引不算数据包。
func TestIsPackRealRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "backups")
	r, err := snapshot.Open(ctx, "p1", repoDir, "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "a"), []byte(strings.Repeat("nagi", 1<<16)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, snapshot.Scheduled, []snapshot.Dir{{Name: "data", Path: data}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	r.Close(ctx)
	packs, indexes := 0, 0
	_ = filepath.WalkDir(repoDir, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(repoDir, full)
		rel = filepath.ToSlash(rel)
		switch {
		case isPack(rel):
			packs++
		case strings.HasPrefix(rel, "x"):
			indexes++
		}
		return nil
	})
	if packs == 0 || indexes == 0 {
		t.Fatalf("数据包 %d 个，索引 %d 个", packs, indexes)
	}
}
