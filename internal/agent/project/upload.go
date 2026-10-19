package project

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 面板上传的容器文件：压缩包经 mesh 传到节点，解压到 containers/<容器>/.upload/files 暂存，
// 下次部署这个容器（useUpload）时替换它的 data/，规则和 git 拉取一样（保留路径不动）。

// maxUploadSize 是解压后的总大小上限，防止压缩炸弹写满磁盘。
const maxUploadSize = 10 << 30

// maxUploadEntries 是压缩包里文件和目录数的上限：几百万个空文件不占多少空间，但会耗尽 inode。
const maxUploadEntries = 200_000

func (p containerPaths) uploadDir() string   { return filepath.Join(p.root, ".upload") }
func (p containerPaths) uploadFiles() string { return filepath.Join(p.uploadDir(), "files") }

// UploadResult 是一次上传的结果。
type UploadResult struct {
	Bytes int64
	Files int
	// Stripped 是自动去掉的唯一顶层目录。
	Stripped string
}

// ReceiveUpload 接收容器的一个压缩包（zip、tar.gz 或 tar），解压后暂存，替换上一次没部署的上传。
func (m *Manager) ReceiveUpload(ctx context.Context, id, container string, r io.Reader) (UploadResult, error) {
	var res UploadResult
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return res, err
	}
	if err := m.hasContainer(ctx, row.Dir, container); err != nil {
		return res, err
	}
	p := paths{root: row.Dir}.container(container)
	// 接收和解压不持有项目锁，可能很慢；放到单独的临时目录，完成后再替换暂存的文件。
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := filepath.Join(p.root, ".upload-"+hex.EncodeToString(suffix[:]))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "archive")
	f, err := os.Create(archive)
	if err != nil {
		return res, err
	}
	res.Bytes, err = io.Copy(f, io.LimitReader(r, maxUploadSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return res, fmt.Errorf("接收压缩包: %w", err)
	}
	if res.Bytes > maxUploadSize {
		return res, invalid("压缩包超过 %d GiB", maxUploadSize>>30)
	}
	out := filepath.Join(tmp, "files")
	if res.Files, err = extract(archive, out); err != nil {
		return res, err
	}
	root := out
	if entries, err := os.ReadDir(out); err == nil && len(entries) == 1 && entries[0].IsDir() {
		res.Stripped = entries[0].Name()
		root = filepath.Join(out, res.Stripped)
	}

	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	if err := os.RemoveAll(p.uploadDir()); err != nil {
		return res, err
	}
	if err := os.MkdirAll(p.uploadDir(), 0o700); err != nil {
		return res, err
	}
	return res, os.Rename(root, p.uploadFiles())
}

// extract 解压到 dst，返回文件数。只解出普通文件和目录，跳过符号链接等其他类型；
// 路径一律限制在 dst 里面。
func extract(archive, dst string) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	x := &extractor{dst: dst}
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		err = x.zip(archive)
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		gz, gerr := gzip.NewReader(bufio.NewReader(f))
		if gerr != nil {
			return 0, fmt.Errorf("解压 gzip: %w", gerr)
		}
		err = x.tar(gz)
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		err = x.tar(f)
	default:
		return 0, errors.New("只支持 zip、tar.gz 和 tar 格式的压缩包")
	}
	if err != nil {
		return 0, err
	}
	if x.files == 0 {
		return 0, errors.New("压缩包里没有文件")
	}
	return x.files, nil
}

type extractor struct {
	dst     string
	files   int
	entries int
	written int64
}

func (x *extractor) count() error {
	x.entries++
	if x.entries > maxUploadEntries {
		return fmt.Errorf("压缩包里的文件和目录超过 %d 个", maxUploadEntries)
	}
	return nil
}

// target 把压缩包里的路径变成 dst 下的路径。".." 等越界的部分在 dst 处截止。
// 返回空字符串表示跳过（根目录本身、macOS 生成的附加文件）。
func (x *extractor) target(name string) string {
	clean := path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if clean == "/" || clean == "/__MACOSX" || strings.HasPrefix(clean, "/__MACOSX/") || path.Base(clean) == ".DS_Store" {
		return ""
	}
	return filepath.Join(x.dst, filepath.FromSlash(clean))
}

func (x *extractor) file(name string, mode os.FileMode, r io.Reader) error {
	if err := x.count(); err != nil {
		return err
	}
	p := x.target(name)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// 项目文件是给容器里的进程读的（进程常以非 root 用户运行），一律保证所有用户可读，
	// 保留可执行位。很多打包工具（例如 Python 的 zipfile）默认写 0600，Windows 打的 zip 没有权限信息。
	perm := mode.Perm() | 0o644
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxUploadSize-x.written+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	x.written += n
	if x.written > maxUploadSize {
		return fmt.Errorf("解压后超过 %d GiB", maxUploadSize>>30)
	}
	x.files++
	return nil
}

func (x *extractor) dir(name string) error {
	if err := x.count(); err != nil {
		return err
	}
	if p := x.target(name); p != "" {
		return os.MkdirAll(p, 0o755)
	}
	return nil
}

func (x *extractor) zip(archive string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("读取 zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := x.dir(f.Name); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("读取 %s: %w", f.Name, err)
			}
			err = x.file(f.Name, mode, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (x *extractor) tar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取 tar: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := x.dir(h.Name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := x.file(h.Name, os.FileMode(h.Mode), tr); err != nil {
				return err
			}
		}
	}
}
