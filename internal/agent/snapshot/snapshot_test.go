package snapshot

import (
	"context"
	"errors"
	"io"
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

	id, err := r.Create(ctx, Scheduled, []Dir{{Name: "data", Path: data}, {Name: "volumes", Path: vols}}, []string{"/data/uploads/"}, map[string]string{"version": "1"})
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
		if _, err := r.Create(ctx, Version, []Dir{{Name: "data", Path: data}}, nil, map[string]string{"version": strconv.Itoa(i)}); err != nil {
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

func TestListAndWalkFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	data, vols := filepath.Join(root, "data"), filepath.Join(root, "volumes")
	write(t, filepath.Join(vols, "db", "sub", "x"), "hello")
	write(t, filepath.Join(vols, "db", "y"), "y")
	write(t, filepath.Join(data, "app"), "app")
	id, err := r.Create(ctx, Manual, []Dir{{Name: "data", Path: data}, {Name: "volumes", Path: vols}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	list, err := r.ListDir(ctx, id, "volumes/db")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range list {
		names[f.Name] = f.IsDir
	}
	if isDir, ok := names["sub"]; !ok || !isDir {
		t.Errorf("sub 应该是目录：%v", names)
	}
	if isDir, ok := names["y"]; !ok || isDir {
		t.Errorf("y 应该是文件：%v", names)
	}
	if f, err := r.Stat(ctx, id, "volumes/db/y"); err != nil || f.IsDir || f.Size != 1 {
		t.Errorf("Stat = %+v, %v", f, err)
	}
	if _, err := r.ListDir(ctx, id, "volumes/db/y"); err == nil {
		t.Error("文件不能当目录列")
	}
	if _, err := r.Stat(ctx, id, "volumes/none"); err == nil {
		t.Error("不存在的路径应该报错")
	}

	content := map[string]string{}
	err = r.WalkFiles(ctx, id, "volumes/db", func(sub string, f FileInfo, open func() (io.ReadCloser, error)) error {
		if open == nil {
			return nil
		}
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		content[sub] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if content["sub/x"] != "hello" || content["y"] != "y" || len(content) != 2 {
		t.Errorf("遍历结果 = %v", content)
	}

	// 单个文件：sub 为空，能读内容。
	single := ""
	err = r.WalkFiles(ctx, id, "volumes/db/y", func(sub string, _ FileInfo, open func() (io.ReadCloser, error)) error {
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		single = sub + "=" + string(b)
		return err
	})
	if err != nil || single != "=y" {
		t.Errorf("单个文件 = %q, %v", single, err)
	}
}

func TestLimitAndDelete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Open(ctx, "p1", filepath.Join(root, "backups"), "test-password", filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	data := filepath.Join(root, "data")
	create := func(kind Kind, content string) string {
		t.Helper()
		write(t, filepath.Join(data, "app.js"), content)
		id, err := r.Create(ctx, kind, []Dir{{Name: "data", Path: data}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// 定时、手动、部署前合计超过上限时删除最老的，版本文件不算。
	r.Limit = 3
	oldest := create(Manual, "1")
	create(PreDeploy, "2")
	ver := create(Version, "3")
	create(Scheduled, "4")
	create(Manual, "5")
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counted := 0
	for _, s := range list {
		if s.ID == oldest {
			t.Fatal("the oldest snapshot should be removed by the limit")
		}
		if s.Kind != Version {
			counted++
		}
	}
	if counted != 3 || len(list) != 4 {
		t.Fatalf("kept %d counted of %d, want 3 of 4", counted, len(list))
	}

	// 手动删除：版本文件不能删。
	if err := r.Delete(ctx, ver); !errors.Is(err, ErrNotDeletable) {
		t.Fatalf("delete version = %v, want ErrNotDeletable", err)
	}
	if err := r.Delete(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if after, _ := r.List(ctx); len(after) != 3 {
		t.Fatalf("after delete = %d, want 3", len(after))
	}
}
