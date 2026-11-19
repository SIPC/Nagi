package fileops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// errNotRegular：要打开的路径已经不是普通文件，没有打开它。
var errNotRegular = errors.New("not a regular file")

// OpenRegular 经 root 打开普通文件 rel，并核对打开的就是 info（之前 Lstat 或遍历目录得到的）描述的那个。
// 容器能改卷里的文件，看过之后、打开之前可能把它换成符号链接、设备文件、管道或别的文件：这时 ok 为 false，
// 不返回文件。不会卡在管道上；Linux 上也不会打开设备文件（见 openNoDevice）。
// 读的时候不要超过 info.Size()：文件可能还在变长。
func OpenRegular(root *os.Root, rel string, info fs.FileInfo) (*os.File, bool, error) {
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	f, err := openNoDevice(root, rel)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, errNotRegular) {
			return nil, false, nil
		}
		return nil, false, err
	}
	cur, err := f.Stat()
	if err != nil || !cur.Mode().IsRegular() || !os.SameFile(cur, info) {
		f.Close()
		return nil, false, err
	}
	return f, true, nil
}

// OpenDir 经 root 打开目录 rel 来读目录项。rel 已经不是目录时报错，不会打开它（打开管道会卡住，
// 打开设备文件可能有副作用）。
func OpenDir(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|oNonblock|oDirectory, 0)
}

// RootFS 和 root.FS() 一样，但读目录时用 OpenDir：fs.WalkDir 遍历时目录被换成管道也不会卡住。
// 遍历到的文件要用 OpenRegular 打开。
func RootFS(root *os.Root) fs.FS { return rootFS{root.FS(), root} }

type rootFS struct {
	fs.FS
	root *os.Root
}

// Stat、Lstat、ReadLink 不打开文件（fs.Stat 这些在没有对应方法时会打开它，或者跟随符号链接）。
func (r rootFS) Stat(name string) (fs.FileInfo, error)  { return fs.Stat(r.FS, name) }
func (r rootFS) Lstat(name string) (fs.FileInfo, error) { return fs.Lstat(r.FS, name) }
func (r rootFS) ReadLink(name string) (string, error)   { return fs.ReadLink(r.FS, name) }

func (r rootFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	d, err := OpenDir(r.root, filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}
