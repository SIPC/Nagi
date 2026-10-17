package project

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func TestSyncDirPreserves(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	// 旧版本：代码 + 运行数据
	write(t, dst, "app.js", "old")
	write(t, dst, "removed.js", "old")
	write(t, dst, "lib/old.js", "old")
	write(t, dst, "uploads/a.png", "user data")
	write(t, dst, "var/db/data.bin", "db")
	write(t, dst, "var/cache/x", "cache")
	// 新版本
	write(t, src, "app.js", "new")
	write(t, src, "lib/new.js", "new")
	write(t, src, "uploads/placeholder", "from repo")

	var logs []string
	preserve := normalizePreserve([]string{"uploads/", "./var/db", "../evil", "/abs"})
	if err := syncDir(src, dst, preserve, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}

	checks := map[string]string{"app.js": "new", "lib/new.js": "new", "uploads/a.png": "user data", "var/db/data.bin": "db"}
	for rel, want := range checks {
		if got, ok := read(t, dst, rel); !ok || got != want {
			t.Errorf("%s = %q (exists %v), want %q", rel, got, ok, want)
		}
	}
	for _, rel := range []string{"removed.js", "lib/old.js", "var/cache/x", "uploads/placeholder"} {
		if _, ok := read(t, dst, rel); ok {
			t.Errorf("%s should not exist", rel)
		}
	}
	if len(logs) != 1 {
		t.Errorf("expected one ignore notice, got %v", logs)
	}
	if len(preserve) != 3 || preserve[2] != "abs" {
		t.Errorf("normalized preserve = %v", preserve)
	}
}
