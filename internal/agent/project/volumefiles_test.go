package project

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestCleanVolumePath(t *testing.T) {
	for in, want := range map[string]string{"": ".", "/": ".", "a/b": filepath.Join("a", "b"), "/a/": "a", "a/./b": filepath.Join("a", "b")} {
		got, err := cleanVolumePath(in)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v，应为 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../x", "a/../../x"} {
		if got, err := cleanVolumePath(in); err == nil {
			t.Errorf("%q 应该被拒绝，得到 %q", in, got)
		}
	}
}

func TestVolumeDirRejectsTraversal(t *testing.T) {
	s := &nagiv1.ContainerSpec{Dir: filepath.Join("srv", "c")}
	for _, v := range []string{"", ".", "..", "a/b", `a\b`, "../../etc"} {
		if _, err := volumeDir(s, v); err == nil {
			t.Errorf("卷名 %q 应该被拒绝", v)
		}
	}
	if got, err := volumeDir(s, DataVolume); err != nil || got != filepath.Join("srv", "c", "data") {
		t.Errorf("data = %q, %v", got, err)
	}
	if got, err := volumeDir(s, "db"); err != nil || got != filepath.Join("srv", "c", "volumes", "db") {
		t.Errorf("db = %q, %v", got, err)
	}
}

func TestWriteRootFileReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := writeRootFile(root, filepath.Join("sub", "f"), bytes.NewReader([]byte("one")), 0o644, -1, -1); err != nil {
		t.Fatal(err)
	}
	if n, err := writeRootFile(root, filepath.Join("sub", "f"), bytes.NewReader([]byte("second")), 0o644, -1, -1); err != nil || n != 6 {
		t.Fatalf("覆盖 = %d, %v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "sub", "f"))
	if string(got) != "second" {
		t.Errorf("内容 = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "sub"))
	if len(entries) != 1 {
		t.Errorf("临时文件没有清理：%v", entries)
	}
}

// 容器把卷里的路径换成指向外面的符号链接，写入不能落到外面。
func TestWriteRootFileDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("创建符号链接需要权限")
	}
	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// 目标是链接：替换链接本身。
	if _, err := writeRootFile(root, "link", bytes.NewReader([]byte("new")), 0o644, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(secret); string(got) != "keep" {
		t.Errorf("外面的文件被改了：%q", got)
	}
	// 上级目录是链接：直接报错。
	if _, err := writeRootFile(root, filepath.Join("linkdir", "x"), bytes.NewReader([]byte("new")), 0o644, -1, -1); err == nil {
		t.Error("经链接目录写到外面应该失败")
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("文件写到外面了")
	}
}

func TestStageUpload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staging")

	// 传到一半断开：不留文件。
	broken := io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errors.New("断开")))
	if _, err := stageUpload(dir, broken); err == nil {
		t.Fatal("读取失败时应该报错")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("不应该留下文件：%v", entries)
	}

	// 节点重启留下的旧文件被清掉，新传的还在。
	stale := filepath.Join(dir, "upload-stale")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * uploadStagingKeep)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	staged, err := stageUpload(dir, strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("旧文件应该被清掉：%v", err)
	}
	if b, err := os.ReadFile(staged); err != nil || string(b) != "new" {
		t.Fatalf("内容不对：%q %v", b, err)
	}
}

func TestTarRootDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var buf bytes.Buffer
	send := func(c *nagiv1.VolumeFileChunk) error {
		buf.Write(c.GetData())
		return nil
	}
	if err := writeArchive(send, func(tw *tar.Writer) error { return tarRootDir(t.Context(), root, "a", tw) }); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = string(data)
	}
	if files["b/f"] != "hello" {
		t.Errorf("打包内容 = %v", files)
	}
	if _, ok := files["b/"]; !ok {
		t.Errorf("缺少目录：%v", files)
	}
}
