package fileops

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// openNoDevice 打开 rel 来读，但不打开普通文件以外的东西：先用 O_PATH 打开（不调用设备驱动的 open，
// 也不会卡在管道上），确认是普通文件以后，再经 /proc/self/fd 重新打开同一个 inode。
// 只看类型再打开不行：两步之间文件能被换掉，而打开某些设备本身就有副作用（例如看门狗）。
func openNoDevice(root *os.Root, rel string) (*os.File, error) {
	p, err := root.OpenFile(rel, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	info, err := p.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(p.Fd())), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		// 没有挂载 /proc：直接打开，由 OpenRegular 核对打开的是不是同一个文件。
		return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
	return f, err
}
