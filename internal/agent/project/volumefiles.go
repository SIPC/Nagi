package project

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SIPC/nagi/internal/agent/snapshot"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 容器存储里的文件管理（见 docs/agent.md#存储）。容器能写这些目录，所以一律经 os.Root：
// 路径解析不会走出根目录，符号链接不跟随，也不把它们当文件读写。

const (
	maxVolumeEntries = 5000
	fileChunkSize    = 256 << 10
)

// volumeDir 返回卷在节点上的目录。不在快照范围的卷不在容器目录里，这里管不了。
func volumeDir(s *nagiv1.ContainerSpec, volume string) (string, error) {
	mp := containerPaths{root: s.GetDir()}
	if volume == DataVolume {
		return mp.dataDir(), nil
	}
	if volume == "" || volume == "." || volume == ".." || strings.ContainsAny(volume, `/\`) {
		return "", invalid("卷名不对")
	}
	return filepath.Join(mp.volumesDir(), volume), nil
}

// cleanVolumePath 把界面传来的路径（/ 分隔，相对卷的根）转成 os.Root 用的相对路径。根目录是 "."。
func cleanVolumePath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return ".", nil
	}
	rel := filepath.FromSlash(p)
	if !filepath.IsLocal(rel) {
		return "", invalid("路径不对：%s", p)
	}
	return filepath.Clean(rel), nil
}

// openVolumeRoot 打开卷的目录。目录本身是符号链接时拒绝。
func openVolumeRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, invalid("卷不存在")
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, invalid("卷不是目录")
	}
	return os.OpenRoot(dir)
}

func (m *Manager) volumeRoot(ctx context.Context, ref *nagiv1.VolumeFileRef) (*os.Root, *nagiv1.ContainerSpec, error) {
	c, err := m.Get(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return nil, nil, err
	}
	dir, err := volumeDir(c.Spec, ref.GetVolume())
	if err != nil {
		return nil, nil, err
	}
	root, err := openVolumeRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	return root, c.Spec, nil
}

// snapshotVolumePath 返回卷里的路径在快照里的位置。快照的顶层是 data 和 volumes。
func snapshotVolumePath(volume, rel string) string {
	base := "data"
	if volume != DataVolume {
		base = "volumes/" + volume
	}
	if rel == "." {
		return base
	}
	return base + "/" + filepath.ToSlash(rel)
}

// ListVolumeFiles 列出卷里（或快照里这个卷的）一个目录。
func (m *Manager) ListVolumeFiles(ctx context.Context, ref *nagiv1.VolumeFileRef) (*nagiv1.ListVolumeFilesResponse, error) {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListVolumeFilesResponse{}
	if ref.GetSnapshotId() != "" {
		c, err := m.Get(ctx, ref.GetProjectId(), ref.GetContainer())
		if err != nil {
			return nil, err
		}
		if _, err := volumeDir(c.Spec, ref.GetVolume()); err != nil {
			return nil, err
		}
		rep, err := m.openRepo(ctx, c.Spec)
		if err != nil {
			return nil, err
		}
		defer rep.Close(ctx)
		list, err := rep.ListDir(ctx, ref.GetSnapshotId(), snapshotVolumePath(ref.GetVolume(), rel))
		if err != nil {
			return nil, invalid("读取快照：%v", err)
		}
		for _, f := range list {
			out.Entries = append(out.Entries, &nagiv1.VolumeFileEntry{
				Name: f.Name, IsDir: f.IsDir, IsSymlink: f.IsSymlink, Size: f.Size, Modified: timestamppb.New(f.Modified),
			})
		}
		sortVolumeEntries(out)
		return out, nil
	}
	root, _, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(rel)
	if err != nil {
		return nil, fileErr(err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxVolumeEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fileErr(err)
	}
	if len(entries) > maxVolumeEntries {
		entries, out.Truncated = entries[:maxVolumeEntries], true
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out.Entries = append(out.Entries, &nagiv1.VolumeFileEntry{
			Name: e.Name(), IsDir: e.IsDir(), IsSymlink: info.Mode()&os.ModeSymlink != 0, Size: info.Size(),
			Modified: timestamppb.New(info.ModTime()), Mode: info.Mode().Perm().String(),
		})
	}
	sortVolumeEntries(out)
	return out, nil
}

func sortVolumeEntries(out *nagiv1.ListVolumeFilesResponse) {
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.GetIsDir() != b.GetIsDir() {
			return a.GetIsDir()
		}
		return a.GetName() < b.GetName()
	})
}

// fileErr 把文件操作的错误转成给人看的说明，错误里不带节点上的路径。
func fileErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return invalid("文件不存在")
	case errors.Is(err, fs.ErrPermission):
		return invalid("没有权限访问这个文件")
	case errors.Is(err, fs.ErrExist):
		return invalid("已经有同名的文件或目录，或路径里有符号链接")
	case err != nil && strings.Contains(err.Error(), "path escapes"):
		return invalid("这个路径经过了指向卷外面的符号链接，已拒绝")
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return invalid("文件操作失败：%v", pe.Err)
	}
	return err
}

// ReadVolumeFile 把卷里（或快照里）的文件发给 send：第一条带文件名和大小，之后是内容。
// 目录打包成 tar.gz，符号链接和特殊文件跳过。
func (m *Manager) ReadVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef, send func(*nagiv1.VolumeFileChunk) error) error {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return err
	}
	if ref.GetSnapshotId() != "" {
		return m.readSnapshotFile(ctx, ref, rel, send)
	}
	root, _, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		return fileErr(err)
	}
	name := downloadName(ref, rel)
	switch {
	case info.Mode().IsRegular():
		f, err := root.Open(rel)
		if err != nil {
			return fileErr(err)
		}
		defer f.Close()
		if err := send(&nagiv1.VolumeFileChunk{Name: name, Size: info.Size()}); err != nil {
			return err
		}
		return sendChunks(f, send)
	case info.IsDir():
		if err := send(&nagiv1.VolumeFileChunk{Name: name + ".tar.gz", Archive: true}); err != nil {
			return err
		}
		return writeArchive(send, func(tw *tar.Writer) error { return tarRootDir(ctx, root, rel, tw) })
	}
	return invalid("这个文件是符号链接或特殊文件，不能下载")
}

func (m *Manager) readSnapshotFile(ctx context.Context, ref *nagiv1.VolumeFileRef, rel string, send func(*nagiv1.VolumeFileChunk) error) error {
	c, err := m.Get(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return err
	}
	if _, err := volumeDir(c.Spec, ref.GetVolume()); err != nil {
		return err
	}
	rep, err := m.openRepo(ctx, c.Spec)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	src := snapshotVolumePath(ref.GetVolume(), rel)
	name := downloadName(ref, rel)
	top, err := rep.Stat(ctx, ref.GetSnapshotId(), src)
	if err != nil {
		return invalid("读取快照：%v", err)
	}
	if !top.IsDir {
		if top.IsSymlink {
			return invalid("这个文件是符号链接或特殊文件，不能下载")
		}
		if err := send(&nagiv1.VolumeFileChunk{Name: name, Size: top.Size}); err != nil {
			return err
		}
		return rep.WalkFiles(ctx, ref.GetSnapshotId(), src, func(_ string, _ snapshot.FileInfo, open func() (io.ReadCloser, error)) error {
			if open == nil {
				return nil
			}
			r, err := open()
			if err != nil {
				return err
			}
			defer r.Close()
			return sendChunks(r, send)
		})
	}
	if err := send(&nagiv1.VolumeFileChunk{Name: name + ".tar.gz", Archive: true}); err != nil {
		return err
	}
	return writeArchive(send, func(tw *tar.Writer) error {
		return rep.WalkFiles(ctx, ref.GetSnapshotId(), src, func(sub string, f snapshot.FileInfo, open func() (io.ReadCloser, error)) error {
			if sub == "" || f.IsSymlink {
				return nil
			}
			h := &tar.Header{Name: sub, Mode: 0o644, Size: f.Size, ModTime: f.Modified, Typeflag: tar.TypeReg}
			if f.IsDir {
				h = &tar.Header{Name: sub + "/", Mode: 0o755, ModTime: f.Modified, Typeflag: tar.TypeDir}
			}
			if err := tw.WriteHeader(h); err != nil || open == nil {
				return err
			}
			r, err := open()
			if err != nil {
				return err
			}
			defer r.Close()
			_, err = io.Copy(tw, r)
			return err
		})
	})
}

// downloadName 是下载时的文件名：文件或目录的名字，卷的根目录用卷名。
func downloadName(ref *nagiv1.VolumeFileRef, rel string) string {
	if rel != "." {
		return filepath.Base(rel)
	}
	if ref.GetVolume() == DataVolume {
		return ref.GetContainer() + "-data"
	}
	return ref.GetVolume()
}

func sendChunks(r io.Reader, send func(*nagiv1.VolumeFileChunk) error) error {
	buf := make([]byte, fileChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(&nagiv1.VolumeFileChunk{Data: append([]byte(nil), buf[:n]...)}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type chunkWriter struct {
	send func(*nagiv1.VolumeFileChunk) error
}

func (w chunkWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += fileChunkSize {
		end := min(off+fileChunkSize, len(p))
		if err := w.send(&nagiv1.VolumeFileChunk{Data: append([]byte(nil), p[off:end]...)}); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

func writeArchive(send func(*nagiv1.VolumeFileChunk) error, fill func(*tar.Writer) error) error {
	gz := gzip.NewWriter(chunkWriter{send})
	tw := tar.NewWriter(gz)
	if err := fill(tw); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// tarRootDir 把 root 里的目录 rel 写进 tar，路径相对 rel。只收目录和普通文件。
func tarRootDir(ctx context.Context, root *os.Root, rel string, tw *tar.Writer) error {
	return fs.WalkDir(root.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的跳过，不让一个文件毁掉整个下载。
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sub := strings.TrimPrefix(strings.TrimPrefix(p, filepath.ToSlash(rel)), "/")
		if rel == "." {
			sub = p
		}
		if sub == "" || sub == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Name: sub + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Typeflag: tar.TypeDir})
		case info.Mode().IsRegular():
			f, err := root.Open(filepath.FromSlash(p))
			if err != nil {
				return nil
			}
			defer f.Close()
			// 边打包边有人写时，文件可能变长，只取打开时的大小，否则 tar 会报错。
			if err := tw.WriteHeader(&tar.Header{Name: sub, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			n, err := io.CopyN(tw, f, info.Size())
			if errors.Is(err, io.EOF) && n < info.Size() {
				return fmt.Errorf("%s 在打包时变短了", sub)
			}
			return err
		}
		return nil
	})
}

// lockForWrite 写文件前拿容器的锁：部署、快照恢复进行中不能改文件。
func (m *Manager) lockForWrite(projectID, container string) (func(), error) {
	l := m.lock(projectID, container)
	if !l.TryLock() {
		return nil, invalid("容器正在进行其他操作，稍后再试")
	}
	return l.Unlock, nil
}

// writeRootFile 原子地写一个文件：先写临时文件，再改名。目标是符号链接时替换链接本身，不写到它指向的地方。
func writeRootFile(root *os.Root, rel string, r io.Reader, mode fs.FileMode, uid, gid int) (int64, error) {
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return 0, fileErr(err)
		}
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tmp := filepath.Join(dir, ".nagi-tmp-"+hex.EncodeToString(suffix))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, fileErr(err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(tmp)
		return 0, err
	}
	_ = root.Chmod(tmp, mode)
	if uid >= 0 {
		_ = root.Chown(tmp, uid, gid) // 不是 root 运行（开发机）时改不了，不影响内容。
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return 0, fileErr(err)
	}
	return n, nil
}

// inheritedOwner 决定新文件的权限和属主：覆盖时沿用原文件，新建时跟随所在目录（容器常以非 root 运行）。
func inheritedOwner(root *os.Root, rel string) (fs.FileMode, int, int) {
	if info, err := root.Lstat(rel); err == nil && info.Mode().IsRegular() {
		uid, gid := fileOwner(info)
		return info.Mode().Perm(), uid, gid
	}
	for dir := filepath.Dir(rel); ; dir = filepath.Dir(dir) {
		if info, err := root.Lstat(dir); err == nil && info.IsDir() {
			uid, gid := fileOwner(info)
			return 0o644, uid, gid
		}
		if dir == "." {
			return 0o644, -1, -1
		}
	}
}

// UploadVolumeFile 把 r 写到卷里的 ref.path，已有的文件被覆盖。
func (m *Manager) UploadVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef, r io.Reader) (int64, error) {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return 0, err
	}
	if rel == "." {
		return 0, invalid("要写明文件名")
	}
	unlock, err := m.lockForWrite(ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return 0, err
	}
	defer unlock()
	root, _, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return 0, invalid("这里已经有一个同名目录")
	}
	mode, uid, gid := inheritedOwner(root, rel)
	return writeRootFile(root, rel, r, mode, uid, gid)
}

// DeleteVolumeFile 删除卷里的文件或目录。不能删卷的根目录。符号链接只删链接本身。
func (m *Manager) DeleteVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef) error {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return err
	}
	if rel == "." {
		return invalid("不能删除卷的根目录")
	}
	unlock, err := m.lockForWrite(ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return err
	}
	defer unlock()
	root, _, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Lstat(rel); err != nil {
		return fileErr(err)
	}
	return fileErr(root.RemoveAll(rel))
}

// CreateVolumeDir 在卷里建目录，属主跟随上级目录。
func (m *Manager) CreateVolumeDir(ctx context.Context, ref *nagiv1.VolumeFileRef) error {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return err
	}
	if rel == "." {
		return invalid("要写明目录名")
	}
	unlock, err := m.lockForWrite(ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return err
	}
	defer unlock()
	root, _, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return err
	}
	defer root.Close()
	_, uid, gid := inheritedOwner(root, rel)
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return fileErr(err)
	}
	if uid >= 0 {
		_ = root.Chown(rel, uid, gid)
	}
	return nil
}

// RestoreVolumeFile 把快照里的文件或目录恢复到卷里原来的位置。只覆盖快照里有的文件，别的不动；
// 符号链接不恢复。经 os.Root 写入，所以卷里被换成指向外面的链接也不会写到外面。
func (m *Manager) RestoreVolumeFile(ctx context.Context, ref *nagiv1.VolumeFileRef) error {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return err
	}
	if ref.GetSnapshotId() == "" {
		return invalid("要指定快照")
	}
	unlock, err := m.lockForWrite(ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return err
	}
	defer unlock()
	root, s, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return err
	}
	defer root.Close()
	rep, err := m.openRepo(ctx, s)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	base := filepath.ToSlash(rel)
	return rep.WalkFiles(ctx, ref.GetSnapshotId(), snapshotVolumePath(ref.GetVolume(), rel), func(sub string, f snapshot.FileInfo, open func() (io.ReadCloser, error)) error {
		if f.IsSymlink {
			return nil
		}
		dest := filepath.FromSlash(path.Join(base, sub))
		if f.IsDir {
			if dest == "." {
				return nil
			}
			return fileErr(root.MkdirAll(dest, 0o755))
		}
		if open == nil {
			return nil
		}
		r, err := open()
		if err != nil {
			return err
		}
		defer r.Close()
		uid, gid := f.UID, f.GID
		_, err = writeRootFile(root, dest, r, f.Mode.Perm(), uid, gid)
		return err
	})
}
