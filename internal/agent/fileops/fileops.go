// Package fileops 是卷文件和节点文件共用的文件操作：复制、放置（处理同名）、压缩、解压、目录大小、分块上传
// （见 docs/agent.md#存储、docs/node-access.md#文件）。
//
// 这里只经 os.Root 读写：路径解析不会走出给定的根目录。只处理目录和普通文件，符号链接、硬链接和特殊文件一律跳过，
// 不跟随也不复制。访问规则（卷的锁和租约、节点的禁止访问和只读）由调用方检查。
package fileops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 同名时的处理（见 nagiv1.VolumeTransferRequest.conflict）。
const (
	ConflictAsk       = ""
	ConflictOverwrite = "overwrite"
	ConflictRename    = "rename"
	ConflictSkip      = "skip"
)

// ValidConflict 判断同名处理的写法对不对。
func ValidConflict(c string) bool {
	switch c {
	case ConflictAsk, ConflictOverwrite, ConflictRename, ConflictSkip:
		return true
	}
	return false
}

func invalid(msg string) error { return status.Error(codes.InvalidArgument, msg) }

// ErrConflict 是目标已经存在、又没有指定怎么处理。调用方事先查过同名，一般走不到。
var ErrConflict = status.Error(codes.AlreadyExists, "目标位置已有同名的文件或目录")

// Progress 是长操作的进度，处理过程中更新，另一个 goroutine 读出来发给面板。
type Progress struct {
	Bytes atomic.Int64
	Files atomic.Int64
}

func (p *Progress) add(n int64) {
	if p != nil {
		p.Bytes.Add(n)
	}
}

func (p *Progress) file() {
	if p != nil {
		p.Files.Add(1)
	}
}

// Options 是复制、压缩、解压的共同设置。不能并发使用。
type Options struct {
	// Skip 对源里的路径（相对源的 Root，/ 分隔）返回 true 时不处理，目录整个跳过。节点文件用它跳过禁止访问的路径。
	Skip func(rel string) bool
	// Guard 检查写入的磁盘剩余空间，每写 64 MiB 查一次。
	Guard *metrics.DiskGuard
	// Progress 记下处理了多少字节和文件。
	Progress *Progress
	// UID、GID 是新建的文件和目录的属主，小于 0 时不改（不是 root 运行时也改不了）。
	UID, GID int
	// Links 是跳过的符号链接、硬链接和特殊文件的个数。
	Links int
}

// NoOwner 是不改属主的 Options 的起点。
func NoOwner() *Options { return &Options{UID: -1, GID: -1} }

func (o *Options) skip(rel string) bool {
	return o != nil && o.Skip != nil && o.Skip(filepath.ToSlash(rel))
}

func (o *Options) writer(w io.Writer) io.Writer {
	if o == nil || o.Guard == nil {
		return w
	}
	return o.Guard.Writer(w)
}

func (o *Options) chown(root *os.Root, rel string) {
	if o != nil && o.UID >= 0 {
		_ = root.Lchown(rel, o.UID, o.GID)
	}
}

// progressReader 一边读一边记进度。
type progressReader struct {
	r io.Reader
	p *Progress
}

func (r progressReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.p.add(int64(n))
	return n, err
}

// TempName 是 dir 里一个不会和别的文件重名的临时名字。容器能看见卷里的临时文件，名字以 .nagi-tmp- 开头好认。
func TempName(dir string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return filepath.Join(dir, ".nagi-tmp-"+hex.EncodeToString(b))
}

// CleanName 检查新名字（改名、压缩包名、解压的目录名）：一级名字，不能是 . 或 ..，不能有 / 和 \。
func CleanName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\\\x00") {
		return "", invalid("名称不能为空，不能是 . 或 ..，也不能包含 / 或 \\")
	}
	if len(n) > 255 {
		return "", invalid("名称过长")
	}
	return n, nil
}

// archiveExts 是带两级后缀的压缩包，加编号时编号放在它们前面：a (2).tar.gz。
var archiveExts = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"}

// SplitExt 把文件名分成主干和后缀。以点开头、没有别的点的（.env）没有后缀。
func SplitExt(name string) (string, string) {
	lower := strings.ToLower(name)
	for _, e := range archiveExts {
		if strings.HasSuffix(lower, e) && len(name) > len(e) {
			return name[:len(name)-len(e)], name[len(name)-len(e):]
		}
	}
	ext := filepath.Ext(name)
	if ext == name || ext == "" {
		return name, ""
	}
	return name[:len(name)-len(ext)], ext
}

// FreeName 返回 root 里和 rel 同一目录、还没有被用的名字：a.txt → a (2).txt、a (3).txt……目录不分后缀。
func FreeName(root *os.Root, rel string, dir bool) (string, error) {
	parent, base := filepath.Dir(rel), filepath.Base(rel)
	stem, ext := SplitExt(base)
	if dir {
		stem, ext = base, ""
	}
	for i := 2; i < 10000; i++ {
		cand := filepath.Join(parent, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := root.Lstat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", ErrConflict
}

// Exists 判断 root 里的 rel 在不在（符号链接本身也算）。
func Exists(root *os.Root, rel string) (bool, error) {
	_, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Place 把一项放到 root 里的 dst，dst 已经存在时按 policy 处理；put(final) 做真正的放置（改名或移入）。
// 返回最终的路径，按 skip 跳过时 placed 为 false。overwrite 先把旧的挪到一边，放好以后再删，放置失败时挪回去，
// 所以中途失败不会两边都丢。dir 表示放的是目录（rename 时不分后缀）。
func Place(root *os.Root, dst, policy string, dir bool, put func(final string) error) (final string, placed bool, err error) {
	exists, err := Exists(root, dst)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return dst, true, put(dst)
	}
	switch policy {
	case ConflictSkip:
		return "", false, nil
	case ConflictRename:
		final, err := FreeName(root, dst, dir)
		if err != nil {
			return "", false, err
		}
		return final, true, put(final)
	case ConflictOverwrite:
		aside := TempName(filepath.Dir(dst))
		if err := root.Rename(dst, aside); err != nil {
			return "", false, err
		}
		if err := put(dst); err != nil {
			_ = root.Rename(aside, dst)
			return "", false, err
		}
		_ = root.RemoveAll(aside)
		return dst, true, nil
	}
	return "", false, ErrConflict
}

// Within 判断 rel 是 base 本身或者在 base 下面（都是 / 分隔、整理过的相对路径或绝对路径）。
func Within(rel, base string) bool {
	if base == "." || base == "" {
		return true
	}
	return rel == base || strings.HasPrefix(rel, strings.TrimSuffix(base, "/")+"/")
}
