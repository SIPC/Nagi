//go:build !linux

package fileops

import "os"

// moveInto 在 Linux 以外（开发机）总是改为复制。
func moveInto(string, string, *os.Root, string) error { return errCrossDevice }
