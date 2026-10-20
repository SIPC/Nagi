package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateListRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	data, vols := filepath.Join(root, "data"), filepath.Join(root, "volumes")
	write(t, filepath.Join(data, "app.js"), "v1")
	write(t, filepath.Join(data, "uploads", "a.png"), "user")
	write(t, filepath.Join(vols, "db", "x"), "db1")

	id, err := r.Create(ctx, Scheduled, []Dir{{"data", data}, {"volumes", vols}}, []string{"/data/uploads/"}, map[string]string{"version": "1"})
	if err != nil {
		t.Fatal(err)
	}

	// 改动之后恢复：内容回到快照时的样子，多出来的文件被删除，被忽略的目录不在快照里。
	write(t, filepath.Join(data, "app.js"), "v2")
	write(t, filepath.Join(data, "new.js"), "new")
	target := filepath.Join(root, "restore")
	if err := r.Restore(ctx, id, "data", target); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "app.js")); string(b) != "v1" {
		t.Fatalf("app.js = %q", b)
	}
	if _, err := os.Stat(filepath.Join(target, "uploads")); !os.IsNotExist(err) {
		t.Fatal("ignored dir should not be in snapshot")
	}
	if err := r.Restore(ctx, id, "volumes", vols); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(vols, "db", "x")); string(b) != "db1" {
		t.Fatalf("volume = %q", b)
	}

	tags, err := r.Tags(ctx, id)
	if err != nil || tags["version"] != "1" || tags["kind"] != "scheduled" {
		t.Fatalf("tags = %v %v", tags, err)
	}

	// 版本文件只保留最近 20 份。
	for i := 0; i < 22; i++ {
		write(t, filepath.Join(data, "app.js"), strconv.Itoa(i))
		if _, err := r.Create(ctx, Version, []Dir{{"data", data}}, nil, map[string]string{"version": strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.List(ctx, Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 20 {
		t.Fatalf("version snapshots kept = %d, want 20", len(list))
	}

	// 重新打开（例如节点重启）仍然能读到。
	r.Close(ctx)
	r2, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close(ctx)
	all, err := r2.List(ctx)
	if err != nil || len(all) != 21 {
		t.Fatalf("all snapshots = %d %v", len(all), err)
	}
}
