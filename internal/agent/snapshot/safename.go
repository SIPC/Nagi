package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kopia/kopia/fs"
)

// 快照里的文件名来自仓库，不可信：副本仓库是源节点写的，源节点有仓库密码，能写出任意的目录项。
// kopia 恢复时直接把名字拼到目标路径后面，不检查，名字里带 .. 或 / 就会写到目标目录外面。
// 所以读快照的目录一律经过 safeDir：遍历时检查每个名字，不合法就报错，整次恢复失败。
// 同一目录里也不能重名：同名的符号链接和目录能让后面的内容顺着链接写到目标目录外面。

// ErrBadName 表示快照里有不合法的文件名。
var ErrBadName = errors.New("快照里有不合法的文件名")

// checkName：不能是空、.、..，不能含 / 或 NUL。反斜杠在 Linux 上是普通字符，不拦。
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w：%q", ErrBadName, name)
	}
	return nil
}

func safeEntry(e fs.Entry) fs.Entry {
	if d, ok := e.(fs.Directory); ok {
		if _, done := d.(safeDir); !done {
			return safeDir{d}
		}
	}
	return e
}

type safeDir struct{ fs.Directory }

func (d safeDir) Child(ctx context.Context, name string) (fs.Entry, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	e, err := d.Directory.Child(ctx, name)
	if err != nil {
		return nil, err
	}
	return safeEntry(e), nil
}

func (d safeDir) Iterate(ctx context.Context) (fs.DirectoryIterator, error) {
	it, err := d.Directory.Iterate(ctx)
	if err != nil {
		return nil, err
	}
	return &safeIter{it: it, seen: map[string]bool{}}, nil
}

type safeIter struct {
	it   fs.DirectoryIterator
	seen map[string]bool
}

func (s *safeIter) Next(ctx context.Context) (fs.Entry, error) {
	e, err := s.it.Next(ctx)
	if err != nil || e == nil {
		return e, err
	}
	name := e.Name()
	if err := checkName(name); err != nil {
		return nil, err
	}
	if s.seen[name] {
		return nil, fmt.Errorf("%w：%q 重复", ErrBadName, name)
	}
	s.seen[name] = true
	return safeEntry(e), nil
}

func (s *safeIter) Close() { s.it.Close() }
