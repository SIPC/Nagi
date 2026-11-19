package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/kopia/kopia/fs"
)

// 快照来源（见 docs/snapshot.md#读取来源）。容器能写 data/、volumes/，随时可能把遍历时看到的普通文件或目录
// 换成符号链接、FIFO 或设备文件（例如用 renameat2(RENAME_EXCHANGE) 不停交换）。kopia 自带的 localfs 按路径打开，
// 跟随符号链接，打开后也不核对是不是遍历时看到的那个，会把节点上的任意文件存进快照；打开 FIFO 时一直卡在 open(2)。
// 所以来源自己实现：
//   - 每个来源目录只开一个 os.Root，遍历、Lstat、打开、读链接都经过它，路径解析不会走出这个目录；
//   - 打开时不跟随符号链接、不阻塞（Linux 上是 O_NOFOLLOW|O_NONBLOCK），打开后 fstat：只接受普通文件和目录，
//     并且必须和遍历时 Lstat 看到的是同一个（os.SameFile），否则这一项按读取失败记下，不进快照；
//   - 符号链接按链接本身存，不跟随；FIFO、套接字、设备文件跳过，和 localfs 一样。
//
// 条目的名字、大小、修改时间、权限、属主和 localfs 给出的一致，上一份快照里没变的文件照样直接复用。

// errChanged 表示打开的不是遍历时看到的那个文件（被换掉了），或者不是普通文件、目录。
var errChanged = errors.New("文件在读取时被替换")

// openSource 打开一个来源目录，返回它的根目录条目，叫 name。用完调用 closer 关闭。
func openSource(dir, name string) (fs.Directory, io.Closer, error) {
	root, info, err := openDirNoFollow(dir)
	if err != nil {
		return nil, nil, err
	}
	return &srcDir{srcEntry{root: root, rel: ".", name: name, info: info}}, root, nil
}

// openDirNoFollow 打开目录 dir 作为根，dir 的最后一级是符号链接时拒绝，不跟随。
// 现有目录里「快照包含的子目录」在用户的原目录里，compose 把原目录挂进容器时（如 ./:/app），容器能把子目录
// 换成指向节点任意目录的链接。上级目录由管理员设置，容器换不掉，照常解析。
// 做法：经上级目录的 os.Root 先 Lstat 最后一级，确认是目录；再经同一个 Root 打开它（不会走出上级目录），
// 打开后核对和 Lstat 看到的是同一个目录：两步之间被换成链接时，打开的是链接指向的目录，核对不过。
func openDirNoFollow(dir string) (*os.Root, os.FileInfo, error) {
	dir = filepath.Clean(dir)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()
	base := filepath.Base(dir)
	want, err := parent.Lstat(base)
	if err != nil {
		return nil, nil, err
	}
	if want.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("目录 %s 是符号链接，为安全起见不纳入快照", dir)
	}
	if !want.IsDir() {
		return nil, nil, fmt.Errorf("%s 不是目录", dir)
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		return nil, nil, err
	}
	info, err := root.Stat(".")
	if err == nil && (!info.IsDir() || !os.SameFile(info, want)) {
		err = fmt.Errorf("%w：%s", errChanged, dir)
	}
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	return root, info, nil
}

// srcEntry 是来源里的一个条目。info 是遍历时 Lstat 的结果，打开时用来核对。
type srcEntry struct {
	root *os.Root
	rel  string // 相对来源目录，用 / 分隔，来源目录本身是 "."
	name string
	info os.FileInfo
}

func (e *srcEntry) Name() string       { return e.name }
func (e *srcEntry) IsDir() bool        { return e.info.IsDir() }
func (e *srcEntry) Mode() os.FileMode  { return e.info.Mode() }
func (e *srcEntry) Size() int64        { return e.info.Size() }
func (e *srcEntry) ModTime() time.Time { return time.Unix(0, e.info.ModTime().UnixNano()) }
func (e *srcEntry) Sys() any           { return nil }

func (e *srcEntry) Owner() fs.OwnerInfo   { return ownerInfo(e.info) }
func (e *srcEntry) Device() fs.DeviceInfo { return deviceInfo(e.info) }

// LocalFilesystemPath 返回空：不让 kopia 按路径再去访问（操作系统快照、动作、估算都不用）。
func (e *srcEntry) LocalFilesystemPath() string { return "" }
func (e *srcEntry) Close()                      {}

// open 经 os.Root 打开条目，不跟随符号链接、不阻塞，再核对打开的是遍历时看到的那个：dir 为 true 时要是目录，否则要是普通文件。
func (e *srcEntry) open(dir bool) (*os.File, error) {
	f, err := e.root.OpenFile(e.rel, os.O_RDONLY|oOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	cur, err := f.Stat()
	if err == nil {
		ok := cur.Mode().IsRegular()
		if dir {
			ok = cur.IsDir()
		}
		if !ok || !os.SameFile(cur, e.info) {
			err = fmt.Errorf("%w：%s", errChanged, e.rel)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// child 按名字 Lstat 一个子条目，生成对应类型的条目。不存在（遍历以后被删掉）时返回 nil。
func (e *srcEntry) child(name string) fs.Entry {
	rel := path.Join(e.rel, name)
	info, err := e.root.Lstat(rel)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		// 读不到的这一项按 kopia 的错误处理策略记下，不影响同一目录里的其他条目。
		return &srcError{srcEntry{root: e.root, rel: rel, name: name, info: emptyInfo{name}}, err}
	}
	c := srcEntry{root: e.root, rel: rel, name: name, info: info}
	switch info.Mode() & os.ModeType {
	case os.ModeDir:
		return &srcDir{c}
	case os.ModeSymlink:
		return &srcSymlink{c}
	case 0:
		return &srcFile{c}
	default:
		// FIFO、套接字、设备文件：不支持的类型，kopia 默认直接跳过。
		return &srcError{c, fs.ErrUnknown}
	}
}

type srcDir struct{ srcEntry }

// Size 和 localfs 一样，目录的大小总是 0。
func (d *srcDir) Size() int64                      { return 0 }
func (d *srcDir) SupportsMultipleIterations() bool { return true }

func (d *srcDir) Child(_ context.Context, name string) (fs.Entry, error) {
	if checkName(name) != nil {
		return nil, fs.ErrEntryNotFound
	}
	c := d.child(name)
	if c == nil {
		return nil, fs.ErrEntryNotFound
	}
	if e, ok := c.(*srcError); ok && !errors.Is(e.err, fs.ErrUnknown) {
		return nil, e.err
	}
	return c, nil
}

func (d *srcDir) Iterate(_ context.Context) (fs.DirectoryIterator, error) {
	f, err := d.open(true)
	if err != nil {
		return nil, fmt.Errorf("读取目录: %w", err)
	}
	return &srcIter{dir: d, f: f}, nil
}

// srcIter 按批读目录里的名字，每个名字经 os.Root Lstat。
type srcIter struct {
	dir   *srcDir
	f     *os.File
	names []string
	i     int
}

func (it *srcIter) Next(_ context.Context) (fs.Entry, error) {
	for {
		if it.i >= len(it.names) {
			names, err := it.f.Readdirnames(100)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if len(names) == 0 {
				return nil, nil
			}
			it.names, it.i = names, 0
		}
		name := it.names[it.i]
		it.i++
		if checkName(name) != nil {
			continue
		}
		if c := it.dir.child(name); c != nil {
			return c, nil
		}
	}
}

func (it *srcIter) Close() { it.f.Close() }

type srcFile struct{ srcEntry }

func (f *srcFile) Open(_ context.Context) (fs.Reader, error) {
	file, err := f.open(false)
	if err != nil {
		return nil, fmt.Errorf("打开文件: %w", err)
	}
	return &srcReader{File: file, e: f.srcEntry}, nil
}

// srcReader 是打开的文件，Entry 返回打开以后 fstat 的结果。
type srcReader struct {
	*os.File
	e srcEntry
}

func (r *srcReader) Entry() (fs.Entry, error) {
	info, err := r.Stat()
	if err != nil {
		return nil, err
	}
	e := r.e
	e.info = info
	return &srcFile{e}, nil
}

type srcSymlink struct{ srcEntry }

// Readlink 读链接本身的内容，经过 os.Root。
func (s *srcSymlink) Readlink(_ context.Context) (string, error) {
	return s.root.Readlink(s.rel)
}

// Resolve 不支持：快照只存链接本身，不跟随。
func (s *srcSymlink) Resolve(_ context.Context) (fs.Entry, error) {
	return nil, errors.New("快照不跟随符号链接")
}

type srcError struct {
	srcEntry
	err error
}

func (e *srcError) ErrorInfo() error { return e.err }

// emptyInfo 是 Lstat 失败的条目的占位信息。
type emptyInfo struct{ name string }

func (i emptyInfo) Name() string       { return i.name }
func (i emptyInfo) Size() int64        { return 0 }
func (i emptyInfo) Mode() os.FileMode  { return 0 }
func (i emptyInfo) ModTime() time.Time { return time.Time{} }
func (i emptyInfo) IsDir() bool        { return false }
func (i emptyInfo) Sys() any           { return nil }

var (
	_ fs.Directory  = (*srcDir)(nil)
	_ fs.File       = (*srcFile)(nil)
	_ fs.Symlink    = (*srcSymlink)(nil)
	_ fs.ErrorEntry = (*srcError)(nil)
	_ fs.Reader     = (*srcReader)(nil)
)
