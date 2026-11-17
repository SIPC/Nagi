package fileops

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// moveInto 用 renameat 把目录 srcDir 里的 name 改名成 root 里的 rel：同一个文件系统时只是改名，大文件也立即完成。
// rel 的上级目录经 root 打开，所以卷里被换成指向外面的符号链接也不会移到外面。跨文件系统时返回 errCrossDevice。
func moveInto(srcDir, name string, root *os.Root, rel string) error {
	src, err := os.Open(srcDir)
	if err != nil {
		return err
	}
	defer src.Close()
	parent, err := root.Open(filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer parent.Close()
	if info, err := parent.Stat(); err != nil || !info.IsDir() {
		return errNotDir
	}
	err = unix.Renameat(int(src.Fd()), name, int(parent.Fd()), filepath.Base(rel))
	if errors.Is(err, unix.EXDEV) {
		return errCrossDevice
	}
	if err != nil {
		return &os.LinkError{Op: "renameat", Old: name, New: filepath.Base(rel), Err: err}
	}
	return nil
}
