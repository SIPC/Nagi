//go:build !linux

package agent

import (
	"context"
	"errors"
	"io"
)

// 节点访问只在 Linux 上提供（checkNodeAccess 已经拦下），这里只是让开发机能编译。
type shellSession struct{}

func startShell(string, uint16, uint16) (*shellSession, error) {
	return nil, errors.New("这个系统不支持节点终端")
}

func (*shellSession) Read([]byte) (int, error)  { return 0, errors.New("不支持") }
func (*shellSession) Write([]byte) (int, error) { return 0, errors.New("不支持") }
func (*shellSession) Resize(uint16, uint16)     {}
func (*shellSession) Wait() int                 { return -1 }
func (*shellSession) Close()                    {}

func runNodeCommand(context.Context, string, string, io.Writer) int { return -1 }
