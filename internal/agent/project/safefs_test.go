package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

func TestLabelFileSnapshotComposeMerge(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"first": "BASE=from-first\nCASH='$literal'\n", "second": "REF=${BASE}\nBASE=second\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	text := "services:\n  normal:\n    image: alpine\n    labels: {BASE: explicit}\n    label_file: [./first, ./second]\n  app:\n    profiles: [audit]\n    image: alpine\n    labels:\n      BASE: explicit\n    label_file: [./first, ./second]\n"
	cp, err := spec.LoadCompose(context.Background(), text, nil, dir, "nagi-test")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := buildOverride(overrideInput{proj: cp, mp: containerPaths{root: dir}, subnet: "172.28.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(ov)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	merged, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{WorkingDir: dir, ConfigFiles: []types.ConfigFile{{Filename: "compose.yml", Content: []byte(text)}, {Filename: "override.yml", Content: data}}}, func(o *loader.Options) {
		o.SetProjectName("nagi-test", true)
		o.Profiles = []string{"audit"}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"normal", "app"} {
		labels := merged.Services[name].Labels
		if labels["BASE"] != "explicit" || labels["REF"] != "from-first" || labels["CASH"] != "$literal" {
			t.Fatalf("snapshot changed label semantics for %s: %v", name, labels)
		}
	}
}

func TestLabelFilesAreBoundedAndConfined(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(out, []byte("secret/key+value="), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "large"), []byte(strings.Repeat("A", (64<<10)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "bad"), []byte("secret/key+value="), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ok"), []byte("A=file\nB=preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var doubling strings.Builder
	doubling.WriteString("V0=x\n")
	for i := 1; i <= 18; i++ {
		fmt.Fprintf(&doubling, "V%d=${V%d}${V%d}\n", i, i-1, i-1)
	}
	for name, text := range map[string]string{
		"expanded":   doubling.String(),
		"cross-file": "VALUE=" + strings.Repeat("${B}", 8000) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"link", "large", "bad", "expanded", "cross-file", "ok"} {
		for _, profile := range []string{"", "    profiles: [audit]\n"} {
			files := "./" + name
			if name == "cross-file" {
				files = "./ok, ./cross-file"
			}
			cp, err := spec.LoadCompose(context.Background(), "services:\n  normal:\n    image: alpine\n  app:\n"+profile+"    image: alpine\n    labels:\n      A: explicit\n    label_file: ["+files+"]\n", nil, data, "nagi-test")
			if err != nil {
				t.Fatal(err)
			}
			ov, err := buildOverride(overrideInput{proj: cp, mp: containerPaths{root: base}, projectID: "p", container: "app"})
			if name != "ok" {
				if err == nil {
					t.Errorf("accepted %s", name)
				} else if strings.Contains(err.Error(), "secret/key+value") {
					t.Errorf("file contents leaked: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				labels := ov["services"].(map[string]any)["app"].(map[string]any)["labels"].(map[string]string)
				if labels["A"] != "explicit" || labels["B"] != "preserved" {
					t.Fatalf("label precedence: %v", labels)
				}
			}
		}
	}
}

// 容器把 data/ 里的路径换成指向外面的符号链接，同步文件时不能写到外面去。
func TestSyncDirDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	for _, d := range []string{outside, filepath.Join(src, "conf"), dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "conf", "evil"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "conf")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	// 保留路径：清理时不会删掉它，第二步复制时 conf 已经存在，以现有内容为准。
	// 不保留：清理时删掉链接本身，再按 src 建真目录。两种情况都不能写到 outside。
	for _, preserve := range [][]string{nil, {"conf/evil"}} {
		_ = syncDir(src, dst, preserve, func(string) {})
		if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
			t.Fatalf("保留路径 %v：文件写到了链接指向的目录", preserve)
		}
		os.RemoveAll(filepath.Join(dst, "conf"))
		os.Symlink(outside, filepath.Join(dst, "conf"))
	}
}

func TestNoSymlinkBelow(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "data", "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "data", "link")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	if err := noSymlinkBelow(base, filepath.Join(base, "data", "real", "x")); err != nil {
		t.Fatalf("普通目录被拒绝：%v", err)
	}
	if err := noSymlinkBelow(base, filepath.Join(base, "data", "link", "x")); err == nil {
		t.Fatal("经过符号链接的路径没有被拒绝")
	}
	if err := noSymlinkBelow(base, "/etc"); err != nil {
		t.Fatalf("不在容器目录下的路径不检查：%v", err)
	}
}

// env_file、secrets 和 configs 的 file 是容器目录下的符号链接时也要拒绝：compose 会跟着链接读宿主机的文件。
func TestCheckMountSourcesFiles(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ok.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "link")); err != nil {
		t.Skip("不能创建符号链接：", err)
	}
	cases := map[string]bool{
		"services:\n  a:\n    image: x\n    env_file: ./ok.env\n":                               true,
		"services:\n  a:\n    image: x\n    env_file: ./link\n":                                 false,
		"services:\n  a:\n    image: x\n    secrets: [s]\nsecrets:\n  s:\n    file: ./link\n":   false,
		"services:\n  a:\n    image: x\n    configs: [c]\nconfigs:\n  c:\n    file: ./link\n":   false,
		"services:\n  a:\n    image: x\n    secrets: [s]\nsecrets:\n  s:\n    file: ./ok.env\n": true,
	}
	for text, ok := range cases {
		proj, err := spec.LoadCompose(context.Background(), text, nil, data, "nagi-test")
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if err := checkMountSources(proj, base); (err == nil) != ok {
			t.Errorf("%s: err = %v", text, err)
		}
	}
}
