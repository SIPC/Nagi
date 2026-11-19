//go:build !linux

package fileops

import "os"

// openNoDevice 打开 rel 来读。O_NONBLOCK：中途被换成管道时不会卡住；换成别的文件由 OpenRegular 核对。
func openNoDevice(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|oNonblock, 0)
}
