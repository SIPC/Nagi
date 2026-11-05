package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 两级的名字（project/postgres）放进快照里的同一个目录，能按这个名字单独恢复；Existing 的目录不存在时报错，不替它创建。
func TestNestedDirs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	pg := filepath.Join(root, "app", "postgres")
	os.MkdirAll(pg, 0o755)
	os.WriteFile(filepath.Join(pg, "PG_VERSION"), []byte("16"), 0o644)
	dirs := []Dir{{Name: "data", Path: filepath.Join(root, "data")}, {Name: "project/postgres", Path: pg, Existing: true}}
	id, err := r.Create(ctx, Manual, dirs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pg, "PG_VERSION"), []byte("broken"), 0o644)
	if err := r.Restore(ctx, id, "project/postgres", pg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(pg, "PG_VERSION")); string(b) != "16" {
		t.Fatalf("restored %q", b)
	}
	missing := filepath.Join(root, "app", "library")
	if _, err := r.Create(ctx, Manual, []Dir{{Name: "project/library", Path: missing, Existing: true}}, nil, nil); err == nil {
		t.Fatal("a missing existing directory should be an error")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("the missing directory must not be created")
	}
}
