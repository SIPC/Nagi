package release

import (
	"os"
	"path/filepath"
	"testing"
)

// 替换前把当前版本留成 .old；新版本起不来时换回去，内容是替换前的那个。
func TestKeepAndRestorePrevious(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "nagi")
	if err := os.WriteFile(exe, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{exe: exe, tmp: exe + ".new"}
	if err := os.WriteFile(p.tmp, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v2" {
		t.Fatalf("替换后应是新版本：%q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "v1" {
		t.Fatalf("应留下旧版本：%q", b)
	}
	if err := restorePrevious(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v1" {
		t.Fatalf("换回后应是旧版本：%q", b)
	}
	// 再升级一次：上一次留下的 .old 被换成这次替换前的版本。
	p = &Prepared{exe: exe, tmp: exe + ".new"}
	_ = os.WriteFile(p.tmp, []byte("v3"), 0o755)
	if _, err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "v1" {
		t.Fatalf(".old 应是这次替换前的版本：%q", b)
	}
}
