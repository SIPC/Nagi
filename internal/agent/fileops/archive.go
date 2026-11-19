package fileops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 压缩包的格式。
const (
	FormatZip   = "zip"
	FormatTarGz = "tar.gz"
	FormatTar   = "tar"
)

// 解压的上限，防止压缩炸弹：文件数和解压后的总大小。磁盘剩余空间另由 Options.Guard 检查。测试时调小。
var (
	MaxExtractFiles       = 100_000
	MaxExtractBytes int64 = 64 << 30
)

// ArchiveExt 是格式对应的文件后缀。
func ArchiveExt(format string) string {
	if format == FormatZip {
		return ".zip"
	}
	return ".tar.gz"
}

// ---- 压缩 ----

// WriteArchive 把 src 里的 names（相对 src 的路径）打包成 format 写进 w。压缩包里每一项从它自己的名字开始
// （a/b/c 打包进去是 c/...）。只收目录和普通文件，符号链接和特殊文件跳过（计入 o.Links），o.Skip 的路径跳过。
func WriteArchive(ctx context.Context, src *os.Root, names []string, format string, w io.Writer, o *Options) error {
	if o == nil {
		o = NoOwner()
	}
	w = o.writer(w)
	var add func(name string, info fs.FileInfo, r io.Reader) error
	var finish func() error
	switch format {
	case FormatZip:
		zw := zip.NewWriter(w)
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, flate.DefaultCompression) })
		add = func(name string, info fs.FileInfo, r io.Reader) error {
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			h.Name = name
			if info.IsDir() {
				h.Name += "/"
				h.Method = zip.Store
			} else {
				h.Method = zip.Deflate
			}
			fw, err := zw.CreateHeader(h)
			if err != nil || r == nil {
				return err
			}
			_, err = io.CopyN(fw, r, info.Size())
			return err
		}
		finish = zw.Close
	case FormatTarGz:
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		add = func(name string, info fs.FileInfo, r io.Reader) error {
			h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Typeflag: tar.TypeReg, Size: info.Size(), Format: tar.FormatPAX}
			if info.IsDir() {
				h.Name, h.Typeflag, h.Size = name+"/", tar.TypeDir, 0
			}
			if err := tw.WriteHeader(h); err != nil || r == nil {
				return err
			}
			_, err := io.CopyN(tw, r, info.Size())
			return err
		}
		finish = func() error {
			if err := tw.Close(); err != nil {
				return err
			}
			return gz.Close()
		}
	default:
		return invalid("压缩格式只能是 zip 或 tar.gz")
	}
	for _, name := range names {
		base := filepath.ToSlash(filepath.Dir(name))
		err := fs.WalkDir(RootFS(src), filepath.ToSlash(name), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == filepath.ToSlash(name) {
					return err
				}
				return nil // 读不了的跳过，不让一个文件毁掉整个压缩包。
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if o.skip(p) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			entry := strings.TrimPrefix(p, base+"/")
			if base == "." {
				entry = p
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			switch {
			case info.IsDir():
				o.Progress.file()
				return add(entry, info, nil)
			case info.Mode().IsRegular():
				f, ok, err := OpenRegular(src, filepath.FromSlash(p), info)
				if err != nil || !ok {
					o.Links++
					return nil
				}
				defer f.Close()
				// 边打包边有人写时，文件可能变长或变短：只取遍历时的大小，不够的补零，否则 tar 会报错。
				r := io.MultiReader(io.LimitReader(progressReader{f, o.Progress}, info.Size()), zeros{})
				o.Progress.file()
				return add(entry, info, r)
			}
			o.Links++
			return nil
		})
		if err != nil {
			return err
		}
	}
	return finish()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// ---- 解压 ----

// Archive 是检查过的压缩包：格式能认，路径都安全，文件数和大小在上限内。
type Archive struct {
	f      *os.File
	size   int64
	format string
	// Top 是压缩包顶层的名字（去重，按出现的顺序），解压到已有目录时按它们查同名。
	Top []string
	// TopDirs 标出顶层的哪些是目录。
	TopDirs map[string]bool
	Files   int
	Bytes   int64
	Links   int
}

func unsafePath(name string) error {
	return status.Errorf(codes.InvalidArgument, "压缩包里有不安全的路径，已拒绝解压：%s", name)
}

var (
	errTooManyFiles = invalid("压缩包里的文件超过 10 万个，已拒绝解压")
	errTooLarge     = invalid("压缩包解压后超过 64 GiB，已拒绝解压")
	errFormat       = invalid("无法识别压缩包，支持 zip、tar.gz（.tgz）和 tar")
)

// entryPath 检查压缩包里的路径：不能是绝对路径（包括 C: 这样的盘符），不能有 ..。返回整理后的 / 分隔路径，
// 空表示压缩包的根目录本身，跳过。
func entryPath(name string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.ContainsRune(n, 0) || strings.HasPrefix(n, "/") || len(n) >= 2 && n[1] == ':' {
		return "", unsafePath(name)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", unsafePath(name)
		}
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// detectFormat 按文件头判断格式，认不出时看后缀（没有 ustar 标记的老式 tar）。
func detectFormat(f *os.File, name string) (string, error) {
	head := make([]byte, 512)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	head = head[:n]
	lower := strings.ToLower(name)
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return FormatZip, nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return FormatTarGz, nil
	case len(head) >= 262 && string(head[257:262]) == "ustar", strings.HasSuffix(lower, ".tar"):
		return FormatTar, nil
	}
	return "", errFormat
}

// IsArchiveName 判断文件名看起来是不是能解压的压缩包（界面据此显示「解压」）。
func IsArchiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, e := range []string{".zip", ".tar.gz", ".tgz", ".tar"} {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}

// ArchiveBase 是压缩包去掉后缀的名字，解压到新目录时用它做目录名。
func ArchiveBase(name string) string {
	lower := strings.ToLower(name)
	for _, e := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(lower, e) && len(name) > len(e) {
			return name[:len(name)-len(e)]
		}
	}
	return name
}

type archiveEntry struct {
	name  string // 整理后的路径
	dir   bool
	mode  fs.FileMode
	size  int64
	mtime time.Time
	open  func() (io.ReadCloser, error) // 普通文件
}

// walk 依次处理压缩包里的每一项。符号链接、硬链接和特殊文件不交给 fn，计入 links。
func (a *Archive) walk(fn func(e archiveEntry) error) (links int, err error) {
	switch a.format {
	case FormatZip:
		zr, err := zip.NewReader(a.f, a.size)
		if err != nil {
			return 0, errFormat
		}
		for _, zf := range zr.File {
			name, err := entryPath(zf.Name)
			if err != nil {
				return links, err
			}
			mode := zf.Mode()
			e := archiveEntry{name: name, mode: mode, mtime: zf.Modified}
			switch {
			case mode.IsDir() || strings.HasSuffix(zf.Name, "/"):
				e.dir = true
			case mode.IsRegular():
				e.size = int64(zf.UncompressedSize64)
				if e.size < 0 {
					return links, errTooLarge
				}
				e.open = zf.Open
			default:
				links++
				continue
			}
			if name == "" {
				continue
			}
			if err := fn(e); err != nil {
				return links, err
			}
		}
		return links, nil
	case FormatTarGz, FormatTar:
		var r io.Reader = io.NewSectionReader(a.f, 0, a.size)
		if a.format == FormatTarGz {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return 0, errFormat
			}
			defer gz.Close()
			r = gz
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return links, nil
			}
			if err != nil {
				return links, invalid("压缩包已损坏，无法解压")
			}
			name, err := entryPath(h.Name)
			if err != nil {
				return links, err
			}
			e := archiveEntry{name: name, mode: fs.FileMode(h.Mode).Perm(), mtime: h.ModTime}
			switch h.Typeflag {
			case tar.TypeDir:
				e.dir = true
			case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
				e.size = h.Size
				e.open = func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }
			case tar.TypeXGlobalHeader:
				continue
			default:
				links++
				continue
			}
			if name == "" {
				continue
			}
			if err := fn(e); err != nil {
				return links, err
			}
		}
	}
	return 0, errFormat
}

// OpenArchive 检查压缩包：认出格式，遍历一遍所有的路径和大小，不安全的路径（绝对路径、..）、超过上限都拒绝。
// 只读不写，所以有问题时什么都还没有写进目标目录。f 在 Archive 用完之前不能关。
func OpenArchive(f *os.File, name string) (*Archive, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	format, err := detectFormat(f, name)
	if err != nil {
		return nil, err
	}
	a := &Archive{f: f, size: info.Size(), format: format, TopDirs: map[string]bool{}}
	seen := map[string]bool{}
	a.Links, err = a.walk(func(e archiveEntry) error {
		a.Files++
		if a.Files > MaxExtractFiles {
			return errTooManyFiles
		}
		a.Bytes += e.size
		if a.Bytes > MaxExtractBytes {
			return errTooLarge
		}
		top, rest, _ := strings.Cut(e.name, "/")
		if !seen[top] {
			seen[top] = true
			a.Top = append(a.Top, top)
		}
		if e.dir || rest != "" {
			a.TopDirs[top] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Extract 把压缩包解压到 dst 里已经存在的目录 dir。文件至少所有人可读（0644），保留可执行位；目录至少 0755。
// 属主按 o.UID、o.GID。中途失败时已经解出的部分由调用方删除（一般解压到临时目录，整个删掉）。
func (a *Archive) Extract(ctx context.Context, dst *os.Root, dir string, o *Options) error {
	if o == nil {
		o = NoOwner()
	}
	var total int64
	files := 0
	type dirTime struct {
		rel   string
		mtime time.Time
	}
	var dirs []dirTime
	links, err := a.walk(func(e archiveEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		files++
		if files > MaxExtractFiles {
			return errTooManyFiles
		}
		rel := filepath.Join(dir, filepath.FromSlash(e.name))
		if e.dir {
			if err := mkdirAll(dst, dir, e.name, o); err != nil {
				return err
			}
			_ = dst.Chmod(rel, e.mode.Perm()|0o755)
			dirs = append(dirs, dirTime{rel, e.mtime})
			o.Progress.file()
			return nil
		}
		if parent := path.Dir(e.name); parent != "." {
			if err := mkdirAll(dst, dir, parent, o); err != nil {
				return err
			}
		}
		r, err := e.open()
		if err != nil {
			return invalid("压缩包已损坏，无法解压")
		}
		defer r.Close()
		// 同一个路径出现两次时后面的覆盖前面的；已有的同名目录不能被文件替换。
		out, err := dst.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		// 多读一个字节：zip 的大小写错时 Open 返回的内容会更长，按实际写入的算。
		n, err := io.Copy(o.writer(out), progressReader{io.LimitReader(r, MaxExtractBytes-total+1), o.Progress})
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		total += n
		if err == nil && total > MaxExtractBytes {
			err = errTooLarge
		}
		if err != nil {
			if errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, io.ErrUnexpectedEOF) {
				return invalid("压缩包已损坏，无法解压")
			}
			return err
		}
		o.chown(dst, rel)
		_ = dst.Chmod(rel, e.mode.Perm()|0o644)
		if !e.mtime.IsZero() {
			_ = dst.Chtimes(rel, e.mtime, e.mtime)
		}
		o.Progress.file()
		return nil
	})
	o.Links += links
	if err != nil {
		return err
	}
	// 目录的修改时间最后设：往里面写文件会改掉它。
	for i := len(dirs) - 1; i >= 0; i-- {
		if !dirs[i].mtime.IsZero() {
			_ = dst.Chtimes(dirs[i].rel, dirs[i].mtime, dirs[i].mtime)
		}
	}
	return nil
}

// mkdirAll 在 dst 的 dir 下面逐级建目录 sub（/ 分隔），新建的目录属主按 o。已有的同名文件不是目录时报错。
func mkdirAll(dst *os.Root, dir, sub string, o *Options) error {
	cur := dir
	for _, seg := range strings.Split(sub, "/") {
		cur = filepath.Join(cur, seg)
		info, err := dst.Lstat(cur)
		if err == nil {
			if !info.IsDir() {
				return status.Errorf(codes.InvalidArgument, "压缩包里的 %s 和同名文件冲突，无法解压", sub)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := dst.Mkdir(cur, 0o755); err != nil {
			return err
		}
		o.chown(dst, cur)
	}
	return nil
}
