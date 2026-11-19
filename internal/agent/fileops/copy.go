package fileops

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyTree 把 src 里的 srcRel（文件或目录）复制成 dst 里的 dstRel，dstRel 不能已经存在，它的上级目录要已经存在。
// 只复制目录和普通文件，符号链接、特殊文件和 o.Skip 的路径跳过（前两种计入 o.Links）。保留权限位和修改时间，
// 属主按 o.UID、o.GID。失败时已经复制的部分由调用方删除（一般复制到临时名字，整个删掉）。
func CopyTree(ctx context.Context, src *os.Root, srcRel string, dst *os.Root, dstRel string, o *Options) error {
	if o == nil {
		o = NoOwner()
	}
	info, err := src.Lstat(srcRel)
	if err != nil {
		return err
	}
	return copyEntry(ctx, src, srcRel, info, dst, dstRel, o)
}

func copyEntry(ctx context.Context, src *os.Root, srcRel string, info fs.FileInfo, dst *os.Root, dstRel string, o *Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		return copyFile(src, srcRel, info, dst, dstRel, o)
	case info.IsDir():
		return copyDir(ctx, src, srcRel, info, dst, dstRel, o)
	}
	o.Links++
	return nil
}

func copyDir(ctx context.Context, src *os.Root, srcRel string, info fs.FileInfo, dst *os.Root, dstRel string, o *Options) error {
	// 先给自己留写权限，内容复制完再改成原来的权限（原目录可能是只读的）。
	if err := dst.Mkdir(dstRel, 0o700); err != nil {
		return err
	}
	o.chown(dst, dstRel)
	d, err := OpenDir(src, srcRel)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(srcRel, e.Name())
		if o.skip(child) {
			continue
		}
		ci, err := e.Info()
		if err != nil {
			continue // 遍历时被删掉了
		}
		if err := copyEntry(ctx, src, child, ci, dst, filepath.Join(dstRel, e.Name()), o); err != nil {
			return err
		}
	}
	_ = dst.Chmod(dstRel, info.Mode().Perm())
	_ = dst.Chtimes(dstRel, info.ModTime(), info.ModTime())
	o.Progress.file()
	return nil
}

func copyFile(src *os.Root, srcRel string, info fs.FileInfo, dst *os.Root, dstRel string, o *Options) error {
	in, ok, err := OpenRegular(src, srcRel, info)
	if err != nil {
		return err
	}
	if !ok {
		o.Links++
		return nil
	}
	defer in.Close()
	out, err := dst.OpenFile(dstRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(o.writer(out), progressReader{in, o.Progress})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	o.chown(dst, dstRel)
	_ = dst.Chmod(dstRel, info.Mode().Perm())
	_ = dst.Chtimes(dstRel, info.ModTime(), info.ModTime())
	o.Progress.file()
	return nil
}
