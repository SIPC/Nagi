package fileops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

var (
	errCrossDevice = errors.New("cross-device")
	errNotDir      = invalid("路径中有一项不是目录")
)

// ErrCrossDevice：Rename 做不到（跨文件系统，或者不是 Linux），调用方改为复制。
var ErrCrossDevice = errCrossDevice

// Rename 把目录 srcDir 里的 name 改名成 root 里的 rel，rel 已经存在时原子地替换（不能替换目录）。
// 做不到时返回 ErrCrossDevice。
func Rename(srcDir, name string, root *os.Root, rel string) error { return moveInto(srcDir, name, root, rel) }

// MoveInto 把目录 srcDir 里的 name（文件或目录）移成 root 里的 rel，rel 不能已经存在，上级目录要已经存在。
// 同一个文件系统时直接改名；否则复制过去再删掉原来的（o 用于复制）。用于把暂存目录里准备好的内容放进卷里。
func MoveInto(ctx context.Context, srcDir, name string, root *os.Root, rel string, o *Options) error {
	err := moveInto(srcDir, name, root, rel)
	if !errors.Is(err, errCrossDevice) {
		return err
	}
	src, err := os.OpenRoot(srcDir)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := CopyTree(ctx, src, name, root, rel, o); err != nil {
		_ = root.RemoveAll(rel)
		return err
	}
	return os.RemoveAll(filepath.Join(srcDir, name))
}
