package fileops

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// procFD 是重新打开 O_PATH 句柄用的目录。测试里换成不存在的目录，模拟没有挂载 /proc。
var procFD = "/proc/self/fd/"

// errNoProc：没有挂载 /proc，没法保证不打开设备，拒绝读取。
var errNoProc = status.Error(codes.FailedPrecondition, "节点没有挂载 /proc，无法安全读取文件")

// openNoDevice 打开 rel 来读，但不打开普通文件以外的东西：先用 O_PATH 打开（不调用设备驱动的 open，
// 也不会卡在管道上），确认是普通文件以后，再经 /proc/self/fd 重新打开同一个 inode。
// 只看类型再打开不行：两步之间文件能被换掉，而打开某些设备本身就有副作用（例如看门狗）。
// 没有挂载 /proc 时拒绝读取，不退回「直接打开再核对」：那样在两步之间仍可能打开设备一次。
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
	f, err := os.OpenFile(procFD+strconv.Itoa(int(p.Fd())), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoProc
	}
	return f, err
}
