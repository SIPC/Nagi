package fileops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// symlink 建符号链接；Windows 上没有权限时跳过测试。
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("无法创建符号链接：%v", err)
	}
}

func TestCopyTree(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "src/a.txt", "a")
	write(t, dir, "src/sub/b.txt", "bb")
	if err := os.Mkdir(filepath.Join(dir, "src", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, dir)
	o := NoOwner()
	o.Progress = &Progress{}
	if err := CopyTree(context.Background(), root, "src", root, "dst", o); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "dst/a.txt") != "a" || read(t, dir, "dst/sub/b.txt") != "bb" {
		t.Fatal("复制的内容不对")
	}
	if info, err := os.Stat(filepath.Join(dir, "dst", "empty")); err != nil || !info.IsDir() {
		t.Fatal("空目录没有复制")
	}
	if o.Progress.Bytes.Load() != 3 {
		t.Fatalf("进度 %d 字节，应该是 3", o.Progress.Bytes.Load())
	}
	// 目标已经存在时不复制。
	if err := CopyTree(context.Background(), root, "src", root, "dst", NoOwner()); err == nil {
		t.Fatal("目标已存在时应该失败")
	}
}

func TestCopyTreeSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "secret", "s")
	write(t, dir, "src/a.txt", "a")
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(dir, "src", "link"))
	symlink(t, outside, filepath.Join(dir, "src", "linkdir"))
	root := openRoot(t, dir)
	o := NoOwner()
	if err := CopyTree(context.Background(), root, "src", root, "dst", o); err != nil {
		t.Fatal(err)
	}
	if o.Links != 2 {
		t.Fatalf("跳过了 %d 个符号链接，应该是 2", o.Links)
	}
	for _, n := range []string{"link", "linkdir"} {
		if _, err := os.Lstat(filepath.Join(dir, "dst", n)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s 不应该被复制", n)
		}
	}
	// 选中的就是一个符号链接：不复制。
	o = NoOwner()
	if err := CopyTree(context.Background(), root, filepath.Join("src", "link"), root, "copied", o); err != nil || o.Links != 1 {
		t.Fatalf("符号链接本身应该被跳过：%v %d", err, o.Links)
	}
	// 经 os.Root 走不出去：指向外面的链接当作路径的一部分时打不开。
	if err := CopyTree(context.Background(), root, filepath.Join("src", "linkdir", "secret"), root, "x", NoOwner()); err == nil {
		t.Fatal("经过指向外面的符号链接应该失败")
	}
}

func TestCopyTreeSkip(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "src/keep.txt", "k")
	write(t, dir, "src/denied/key", "secret")
	root := openRoot(t, dir)
	o := NoOwner()
	o.Skip = func(rel string) bool { return rel == "src/denied" }
	if err := CopyTree(context.Background(), root, "src", root, "dst", o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dst", "denied")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Skip 的路径不应该被复制")
	}
}

func TestSplitExtAndFreeName(t *testing.T) {
	cases := map[string][2]string{
		"a.txt": {"a", ".txt"}, "a.tar.gz": {"a", ".tar.gz"}, ".env": {".env", ""}, "Makefile": {"Makefile", ""}, "x.y.zip": {"x.y", ".zip"},
	}
	for in, want := range cases {
		if s, e := SplitExt(in); s != want[0] || e != want[1] {
			t.Errorf("SplitExt(%q) = %q %q", in, s, e)
		}
	}
	dir := t.TempDir()
	write(t, dir, "a.tar.gz", "")
	write(t, dir, "a (2).tar.gz", "")
	root := openRoot(t, dir)
	if got, _ := FreeName(root, "a.tar.gz", false); got != "a (3).tar.gz" {
		t.Fatalf("FreeName = %q", got)
	}
	if got, _ := FreeName(root, "v1.0", true); got != "v1.0 (2)" {
		t.Fatalf("目录不分后缀：%q", got)
	}
}

func TestPlace(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	put := func(content string) func(string) error {
		return func(final string) error {
			return root.WriteFile(final, []byte(content), 0o644)
		}
	}
	write(t, dir, "f.txt", "old")
	if _, _, err := Place(root, "f.txt", ConflictAsk, false, put("new")); !errors.Is(err, ErrConflict) {
		t.Fatalf("没有指定处理方式时应该拒绝：%v", err)
	}
	if _, placed, err := Place(root, "f.txt", ConflictSkip, false, put("new")); err != nil || placed || read(t, dir, "f.txt") != "old" {
		t.Fatal("skip 不应该改动")
	}
	if final, _, err := Place(root, "f.txt", ConflictRename, false, put("new")); err != nil || final != "f (2).txt" || read(t, dir, "f (2).txt") != "new" {
		t.Fatalf("rename：%q %v", final, err)
	}
	if _, _, err := Place(root, "f.txt", ConflictOverwrite, false, put("new")); err != nil || read(t, dir, "f.txt") != "new" {
		t.Fatalf("overwrite：%v", err)
	}
	// 放置失败时旧的放回去。
	write(t, dir, "g.txt", "keep")
	if _, _, err := Place(root, "g.txt", ConflictOverwrite, false, func(string) error { return errors.New("boom") }); err == nil || read(t, dir, "g.txt") != "keep" {
		t.Fatal("放置失败时应该恢复旧文件")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nagi-tmp-") {
			t.Fatalf("留下了临时文件 %s", e.Name())
		}
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	for _, format := range []string{FormatZip, FormatTarGz} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "vol/site/index.html", "<h1>hi</h1>")
			write(t, dir, "vol/site/css/a.css", "body{}")
			write(t, dir, "vol/readme.md", "# readme")
			big := strings.Repeat("0123456789", 100000)
			write(t, dir, "vol/site/big.bin", big)
			root := openRoot(t, filepath.Join(dir, "vol"))
			var buf bytes.Buffer
			o := NoOwner()
			if err := WriteArchive(context.Background(), root, []string{"site", "readme.md"}, format, &buf, o); err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(dir, "out"+ArchiveExt(format))
			if err := os.WriteFile(f, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			af, _ := os.Open(f)
			defer af.Close()
			a, err := OpenArchive(af, f)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(a.Top)
			if !slices.Equal(a.Top, []string{"readme.md", "site"}) || !a.TopDirs["site"] || a.TopDirs["readme.md"] {
				t.Fatalf("顶层：%v %v", a.Top, a.TopDirs)
			}
			out := filepath.Join(dir, "x")
			os.Mkdir(out, 0o755)
			dst := openRoot(t, dir)
			if err := a.Extract(context.Background(), dst, "x", NoOwner()); err != nil {
				t.Fatal(err)
			}
			if read(t, out, "site/index.html") != "<h1>hi</h1>" || read(t, out, "site/css/a.css") != "body{}" || read(t, out, "readme.md") != "# readme" || read(t, out, "site/big.bin") != big {
				t.Fatal("解压的内容不对")
			}
		})
	}
}

// 子目录里的一项打包进去从它自己的名字开始。
func TestArchiveNestedName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/b/c/file", "x")
	root := openRoot(t, dir)
	var buf bytes.Buffer
	if err := WriteArchive(context.Background(), root, []string{filepath.Join("a", "b", "c")}, FormatZip, &buf, NoOwner()); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"c/", "c/file"}) {
		t.Fatalf("压缩包里的路径：%v", names)
	}
}

func TestWriteArchiveSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "secret", "s")
	write(t, dir, "d/a", "a")
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(dir, "d", "link"))
	root := openRoot(t, dir)
	var buf bytes.Buffer
	o := NoOwner()
	if err := WriteArchive(context.Background(), root, []string{"d"}, FormatTarGz, &buf, o); err != nil {
		t.Fatal(err)
	}
	if o.Links != 1 {
		t.Fatalf("跳过了 %d 个符号链接", o.Links)
	}
	gz, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if strings.Contains(h.Name, "link") {
			t.Fatal("符号链接不应该打包")
		}
	}
}

type entry struct {
	name, body string
	typ        byte
	link       string
}

func makeTar(t *testing.T, path string, entries []entry, gz bool) {
	t.Helper()
	var buf bytes.Buffer
	var w = &buf
	tw := tar.NewWriter(w)
	if gz {
		g := gzip.NewWriter(w)
		tw = tar.NewWriter(g)
		defer func() {
			tw.Close()
			g.Close()
			os.WriteFile(path, buf.Bytes(), 0o644)
		}()
	} else {
		defer func() {
			tw.Close()
			os.WriteFile(path, buf.Bytes(), 0o644)
		}()
	}
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
}

func makeZip(t *testing.T, path string, names ...string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	zw.Close()
	os.WriteFile(path, buf.Bytes(), 0o644)
}

func isInvalid(err error) bool { return status.Code(err) == codes.InvalidArgument }

func TestExtractRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]func(p string){
		"zip-slip.zip":  func(p string) { makeZip(t, p, "ok.txt", "../evil.txt") },
		"abs.zip":       func(p string) { makeZip(t, p, "/etc/evil") },
		"drive.zip":     func(p string) { makeZip(t, p, `C:\evil`) },
		"backslash.zip": func(p string) { makeZip(t, p, `a\..\..\evil`) },
		"slip.tar.gz":   func(p string) { makeTar(t, p, []entry{{name: "a/../../evil", body: "x"}}, true) },
		"abs.tar":       func(p string) { makeTar(t, p, []entry{{name: "/tmp/evil", body: "x"}}, false) },
		"deep-slip.tar": func(p string) {
			makeTar(t, p, []entry{{name: "ok/", typ: tar.TypeDir}, {name: "ok/../../../evil", body: "x"}}, false)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			mk(p)
			f, _ := os.Open(p)
			defer f.Close()
			if _, err := OpenArchive(f, p); !isInvalid(err) || !strings.Contains(status.Convert(err).Message(), "不安全的路径") {
				t.Fatalf("应该拒绝：%v", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("写到了目录外面")
	}
}

func TestExtractSkipsLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "links.tar")
	makeTar(t, p, []entry{
		{name: "a.txt", body: "a"},
		{name: "evil", typ: tar.TypeSymlink, link: "/etc/passwd"},
		{name: "hard", typ: tar.TypeLink, link: "a.txt"},
		{name: "dev", typ: tar.TypeChar},
		{name: "fifo", typ: tar.TypeFifo},
		// 链接后面再写同名文件：只解出普通文件，不会经过链接写到外面。
		{name: "evil/x", body: "x"},
	}, false)
	f, _ := os.Open(p)
	defer f.Close()
	a, err := OpenArchive(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Links != 4 {
		t.Fatalf("应该跳过 4 项，跳过了 %d", a.Links)
	}
	os.Mkdir(filepath.Join(dir, "out"), 0o755)
	root := openRoot(t, dir)
	o := NoOwner()
	if err := a.Extract(context.Background(), root, "out", o); err != nil {
		t.Fatal(err)
	}
	if o.Links != 4 {
		t.Fatalf("Extract 记下跳过 %d 项", o.Links)
	}
	if info, err := os.Lstat(filepath.Join(dir, "out", "evil")); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatal("evil 应该是普通目录")
	}
	for _, n := range []string{"hard", "dev", "fifo"} {
		if _, err := os.Lstat(filepath.Join(dir, "out", n)); err == nil {
			t.Fatalf("%s 不应该被解出", n)
		}
	}
}

func TestExtractLimits(t *testing.T) {
	dir := t.TempDir()
	oldFiles, oldBytes := MaxExtractFiles, MaxExtractBytes
	t.Cleanup(func() { MaxExtractFiles, MaxExtractBytes = oldFiles, oldBytes })

	p := filepath.Join(dir, "many.zip")
	makeZip(t, p, "1", "2", "3", "4")
	MaxExtractFiles = 3
	f, _ := os.Open(p)
	defer f.Close()
	if _, err := OpenArchive(f, p); !isInvalid(err) || !strings.Contains(status.Convert(err).Message(), "10 万") {
		t.Fatalf("文件数超过上限应该拒绝：%v", err)
	}
	MaxExtractFiles = oldFiles

	p2 := filepath.Join(dir, "big.tar.gz")
	makeTar(t, p2, []entry{{name: "a", body: strings.Repeat("a", 600)}, {name: "b", body: strings.Repeat("b", 600)}}, true)
	MaxExtractBytes = 1000
	f2, _ := os.Open(p2)
	defer f2.Close()
	if _, err := OpenArchive(f2, p2); !isInvalid(err) || !strings.Contains(status.Convert(err).Message(), "64 GiB") {
		t.Fatalf("解压后超过上限应该拒绝：%v", err)
	}
}

func TestExtractCorruptAndUnknown(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.zip")
	os.WriteFile(p, []byte("not an archive"), 0o644)
	f, _ := os.Open(p)
	defer f.Close()
	if _, err := OpenArchive(f, p); !isInvalid(err) {
		t.Fatalf("认不出的格式应该拒绝：%v", err)
	}
}

func TestDirSizes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/1", "12345")
	write(t, dir, "a/sub/2", "123")
	write(t, dir, "b/1", "1")
	for i := range 50 {
		write(t, dir, "many/"+strings.Repeat("x", i+1), "1")
	}
	fsys := os.DirFS(dir)
	got := DirSizes(context.Background(), fsys, ".", []string{"a", "b"}, time.Second, 1000, 0, nil)
	if got[0].Size != 8 || got[0].Files != 2 || !got[0].Complete || got[1].Size != 1 {
		t.Fatalf("大小不对：%+v", got)
	}
	// 文件数的限额用完时标记为不完整。
	got = DirSizes(context.Background(), fsys, ".", []string{"many"}, time.Second, 10, 0, nil)
	if got[0].Complete || got[0].Files >= 50 {
		t.Fatalf("超出限额应该不完整：%+v", got[0])
	}
	// 时间到了也一样。
	got = DirSizes(context.Background(), fsys, ".", []string{"many"}, 0, 1000, 0, nil)
	if got[0].Complete {
		t.Fatalf("超时应该不完整：%+v", got[0])
	}
	got = DirSizes(context.Background(), fsys, ".", []string{"a"}, time.Second, 1000, 0, func(p string) bool { return p == "a/sub" })
	if got[0].Size != 5 {
		t.Fatalf("跳过的路径不计入：%+v", got[0])
	}
	// 超过 maxBytes 停下，标为太大；没超过的照常统计完。
	got = DirSizes(context.Background(), fsys, ".", []string{"a", "b"}, time.Second, 1000, 4, nil)
	if !got[0].TooLarge || got[0].Complete || got[1].TooLarge || !got[1].Complete {
		t.Fatalf("超过上限的应标为太大：%+v", got)
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestUploadResume(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("nagi-"), 5000) // 25000 字节
	chunks := [][]byte{content[:10000], content[10000:20000], content[20000:]}
	u := &Uploads{Dir: dir}
	info, err := u.Begin("vol/a.bin", "key1", int64(len(content)))
	if err != nil || info.Offset != 0 {
		t.Fatal(err)
	}
	off, err := u.Append(info.ID, "vol/a.bin", 0, sum(chunks[0]), bytes.NewReader(chunks[0]))
	if err != nil || off != 10000 {
		t.Fatalf("第一块：%d %v", off, err)
	}
	// 传第二块时断了：节点只写了一半（模拟写入中途重启：data 比 meta 记的长）。
	data := filepath.Join(dir, info.ID, "data")
	fh, _ := os.OpenFile(data, os.O_WRONLY|os.O_APPEND, 0)
	fh.Write(chunks[1][:3000])
	fh.Close()

	// 节点重启（新的 Uploads），同一个文件重新选择：接着传。
	u = &Uploads{Dir: dir}
	again, err := u.Begin("vol/a.bin", "key1", int64(len(content)))
	if err != nil || again.ID != info.ID || again.Offset != 10000 {
		t.Fatalf("应该从 10000 接着传：%+v %v", again, err)
	}
	// 位置不对的块被拒绝。
	if _, err := u.Append(info.ID, "vol/a.bin", 0, sum(chunks[0]), bytes.NewReader(chunks[0])); !errors.Is(err, ErrUploadOffset) {
		t.Fatalf("位置不对应该拒绝：%v", err)
	}
	// 块的哈希不对被拒绝，位置不变。
	bad := append([]byte(nil), chunks[1]...)
	bad[0] ^= 1
	if _, err := u.Append(info.ID, "vol/a.bin", 10000, sum(chunks[1]), bytes.NewReader(bad)); status.Code(err) != codes.DataLoss {
		t.Fatalf("块的哈希不对应该拒绝：%v", err)
	}
	// 目标不一致不能用。
	if _, err := u.Append(info.ID, "vol/other", 10000, sum(chunks[1]), bytes.NewReader(chunks[1])); err == nil {
		t.Fatal("目标不一致应该拒绝")
	}
	for i, c := range chunks[1:] {
		start := int64(10000 * (i + 1))
		if _, err := u.Append(info.ID, "vol/a.bin", start, sum(c), bytes.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}
	// 整个文件的哈希不对：作废。
	if _, _, _, err := u.Finish(info.ID, "vol/a.bin", sum([]byte("wrong"))); status.Code(err) != codes.DataLoss {
		t.Fatalf("整个文件的哈希不对应该拒绝：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, info.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("校验不一致的上传应该删掉")
	}
}

func TestUploadFinish(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello world")
	u := &Uploads{Dir: dir}
	info, _ := u.Begin("t", "k", int64(len(content)))
	if _, _, _, err := u.Finish(info.ID, "t", sum(content)); err == nil {
		t.Fatal("没传完不能提交")
	}
	if _, err := u.Append(info.ID, "t", 0, sum(content), bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Append(info.ID, "t", int64(len(content)), sum([]byte("x")), bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("超过文件大小应该拒绝")
	}
	p, size, release, err := u.Finish(info.ID, "t", sum(content))
	if err != nil || size != int64(len(content)) {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, content) {
		t.Fatal("内容不对")
	}
	u.RemoveFinished(info.ID)
	release()
	// 换了文件（同一个目标、不同的标识）：旧的上传删掉，从头开始。
	a, _ := u.Begin("t2", "k1", 10)
	b, _ := u.Begin("t2", "k2", 10)
	if a.ID == b.ID || b.Offset != 0 {
		t.Fatal("换了文件应该从头开始")
	}
	if _, err := os.Stat(filepath.Join(dir, a.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("同一个目标的旧上传应该删掉")
	}
	if err := u.Remove(b.ID, "t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Append(b.ID, "t2", 0, sum([]byte("x")), bytes.NewReader([]byte("x"))); status.Code(err) != codes.NotFound {
		t.Fatalf("放弃以后不能再传：%v", err)
	}
	if _, err := u.Append("../../etc", "t2", 0, "", bytes.NewReader(nil)); status.Code(err) != codes.NotFound {
		t.Fatal("ID 只能是十六进制")
	}
}
