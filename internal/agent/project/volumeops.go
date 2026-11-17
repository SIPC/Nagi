package project

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/fileops"
	"github.com/SIPC/nagi/internal/agent/metrics"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 卷里的改名、移动、复制、压缩、解压、目录大小和分块上传（见 docs/agent.md#存储）。
// 读写都经 os.Root，符号链接不跟随也不复制。耗时的部分（复制、打包、解压）先在容器目录的 .staging/ 里做好，
// 这时不占容器的锁；做好以后拿到锁、确认还持有租约（beforeCommit），再改名放进卷里，不挡部署和快照，
// 中途失败也不会在卷里留下半成品。

const maxDirSizeNames = 5000

// opStaging 在容器目录的 .staging/ 里建一个这次操作用的临时目录，返回目录和删除它的函数。
func opStaging(s *nagiv1.ContainerSpec) (string, func(), error) {
	base := containerPaths{root: s.GetDir()}.stagingDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "op-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// ownerOf 是放到 rel 的新内容的属主：跟随所在目录（容器常以非 root 运行）。
func ownerOf(root *os.Root, rel string) (int, int) {
	for dir := filepath.Dir(rel); ; dir = filepath.Dir(dir) {
		if info, err := root.Lstat(dir); err == nil && info.IsDir() {
			return fileOwner(info)
		}
		if dir == "." {
			return -1, -1
		}
	}
}

// precheckWrite 在开始耗时的部分之前确认现在能写：部署、恢复进行中直接提示，免得白做。
func (m *Manager) precheckWrite(ctx context.Context, projectID, container string) error {
	unlock, err := m.lockForWriteWait(ctx, projectID, container, 10*time.Second)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

func volumeRef(projectID, container, volume string) *nagiv1.VolumeFileRef {
	return &nagiv1.VolumeFileRef{ProjectId: projectID, Container: container, Volume: volume}
}

type transferItem struct {
	src, dst string
	dir      bool
	same     bool // 目标就是源本身
}

func slash(p string) string { return filepath.ToSlash(p) }

// cleanSources 整理要处理的源路径：不能是卷的根目录。
func cleanSources(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, invalid("请选择文件")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := cleanVolumePath(p)
		if err != nil {
			return nil, err
		}
		if rel == "." {
			return nil, invalid("不能选择卷的根目录")
		}
		out = append(out, rel)
	}
	return out, nil
}

// TransferVolumeFiles 在同一个卷里移动、复制或改名。conflict 为空时先查同名，有同名就只返回它们，不做改动。
// 复制时选中的符号链接和目录里的符号链接都跳过；复制到原来的目录时自动保留两者。
func (m *Manager) TransferVolumeFiles(ctx context.Context, req *nagiv1.VolumeTransferRequest, prog *fileops.Progress, beforeCommit func() error) (*nagiv1.FileOpResult, error) {
	if prog == nil {
		prog = &fileops.Progress{}
	}
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	srcs, err := cleanSources(req.GetPaths())
	if err != nil {
		return nil, err
	}
	destDir, err := cleanVolumePath(req.GetDestDir())
	if err != nil {
		return nil, err
	}
	newName := ""
	if req.GetNewName() != "" {
		if len(srcs) != 1 {
			return nil, invalid("只能给一项改名")
		}
		if newName, err = fileops.CleanName(req.GetNewName()); err != nil {
			return nil, err
		}
	}
	if err := m.precheckWrite(ctx, req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	root, s, err := m.volumeRoot(ctx, volumeRef(req.GetProjectId(), req.GetContainer(), req.GetVolume()))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if info, err := root.Lstat(destDir); err != nil || !info.IsDir() {
		return nil, invalid("目标目录不存在")
	}
	res := &nagiv1.FileOpResult{}
	var items []transferItem
	for _, src := range srcs {
		info, err := root.Lstat(src)
		if err != nil {
			return nil, fileErr(err)
		}
		name := filepath.Base(src)
		if newName != "" {
			name = newName
		}
		dst := filepath.Join(destDir, name)
		if info.IsDir() && dst != src && fileops.Within(slash(dst), slash(src)) {
			return nil, invalid("不能把目录移动或复制到它自己里面")
		}
		if dst != src && fileops.Within(slash(src), slash(dst)) && policy == fileops.ConflictOverwrite {
			return nil, invalid("不能用目录里的内容覆盖这个目录")
		}
		if req.GetCopy() && !info.IsDir() && !info.Mode().IsRegular() {
			res.SkippedLinks++
			continue
		}
		it := transferItem{src: src, dst: dst, dir: info.IsDir(), same: dst == src}
		if it.same && !req.GetCopy() {
			res.Paths = append(res.Paths, slash(src)) // 移到原处：什么都不用做
			continue
		}
		items = append(items, it)
	}
	if policy == fileops.ConflictAsk {
		for _, it := range items {
			if it.same {
				continue
			}
			if ok, err := fileops.Exists(root, it.dst); err != nil {
				return nil, fileErr(err)
			} else if ok {
				res.Conflicts = append(res.Conflicts, filepath.Base(it.dst))
			}
		}
		if len(res.Conflicts) > 0 {
			return res, nil
		}
	}

	if !req.GetCopy() {
		unlock, err := m.lockForWriteWait(ctx, req.GetProjectId(), req.GetContainer(), uploadLockWait)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if err := beforeCommit(); err != nil {
			return nil, err
		}
		for _, it := range items {
			final, placed, err := fileops.Place(root, it.dst, policy, it.dir, func(final string) error { return root.Rename(it.src, final) })
			if err != nil {
				return res, fileErr(err)
			}
			if !placed {
				res.Skipped++
				continue
			}
			prog.Files.Add(1)
			res.Paths = append(res.Paths, slash(final))
		}
		return res, nil
	}

	stage, cleanup, err := opStaging(s)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return nil, err
	}
	defer stageRoot.Close()
	guard := &metrics.DiskGuard{Dir: stage}
	for i, it := range items {
		uid, gid := ownerOf(root, it.dst)
		o := &fileops.Options{UID: uid, GID: gid, Guard: guard, Progress: prog}
		if err := fileops.CopyTree(ctx, root, it.src, stageRoot, strconv.Itoa(i), o); err != nil {
			return nil, fileErr(err)
		}
		res.SkippedLinks += int32(o.Links)
	}
	unlock, err := m.lockForWriteWait(ctx, req.GetProjectId(), req.GetContainer(), uploadLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := beforeCommit(); err != nil {
		return nil, err
	}
	for i, it := range items {
		p := policy
		if it.same {
			p = fileops.ConflictRename
		}
		final, placed, err := fileops.Place(root, it.dst, p, it.dir, func(final string) error {
			return fileops.MoveInto(ctx, stage, strconv.Itoa(i), root, final, fileops.NoOwner())
		})
		if err != nil {
			return res, fileErr(err)
		}
		if !placed {
			res.Skipped++
			continue
		}
		res.Paths = append(res.Paths, slash(final))
	}
	return res, nil
}

// CompressVolumeFiles 把卷里的几项打包成 dest（zip 或 tar.gz）。符号链接和特殊文件不打包。
func (m *Manager) CompressVolumeFiles(ctx context.Context, req *nagiv1.VolumeCompressRequest, prog *fileops.Progress, beforeCommit func() error) (*nagiv1.FileOpResult, error) {
	if prog == nil {
		prog = &fileops.Progress{}
	}
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	if req.GetFormat() != fileops.FormatZip && req.GetFormat() != fileops.FormatTarGz {
		return nil, invalid("压缩格式只能是 zip 或 tar.gz")
	}
	srcs, err := cleanSources(req.GetPaths())
	if err != nil {
		return nil, err
	}
	dest, err := cleanVolumePath(req.GetDest())
	if err != nil {
		return nil, err
	}
	if dest == "." {
		return nil, invalid("请填写压缩包名称")
	}
	if _, err := fileops.CleanName(filepath.Base(dest)); err != nil {
		return nil, err
	}
	if err := m.precheckWrite(ctx, req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	root, s, err := m.volumeRoot(ctx, volumeRef(req.GetProjectId(), req.GetContainer(), req.GetVolume()))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, src := range srcs {
		if _, err := root.Lstat(src); err != nil {
			return nil, fileErr(err)
		}
	}
	if info, err := root.Lstat(filepath.Dir(dest)); err != nil || !info.IsDir() {
		return nil, invalid("目标目录不存在")
	}
	res := &nagiv1.FileOpResult{}
	if policy == fileops.ConflictAsk {
		if ok, err := fileops.Exists(root, dest); err != nil {
			return nil, fileErr(err)
		} else if ok {
			res.Conflicts = []string{filepath.Base(dest)}
			return res, nil
		}
	}
	stage, cleanup, err := opStaging(s)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := os.OpenFile(filepath.Join(stage, "archive"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	o := &fileops.Options{UID: -1, GID: -1, Guard: &metrics.DiskGuard{Dir: stage}, Progress: prog}
	err = fileops.WriteArchive(ctx, root, srcs, req.GetFormat(), out, o)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fileErr(err)
	}
	res.SkippedLinks = int32(o.Links)
	uid, gid := ownerOf(root, dest)
	_ = os.Chmod(filepath.Join(stage, "archive"), 0o644)
	if uid >= 0 {
		_ = os.Chown(filepath.Join(stage, "archive"), uid, gid)
	}
	unlock, err := m.lockForWriteWait(ctx, req.GetProjectId(), req.GetContainer(), uploadLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := beforeCommit(); err != nil {
		return nil, err
	}
	final, placed, err := fileops.Place(root, dest, policy, false, func(final string) error {
		return fileops.MoveInto(ctx, stage, "archive", root, final, &fileops.Options{UID: uid, GID: gid})
	})
	if err != nil {
		return nil, fileErr(err)
	}
	if !placed {
		res.Skipped++
	} else {
		res.Paths = []string{slash(final)}
	}
	return res, nil
}

// openVolumeArchive 打开卷里的压缩包并检查它（见 fileops.OpenArchive）。
func openVolumeArchive(root *os.Root, rel string) (*os.File, *fileops.Archive, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, fileErr(err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, invalid("该路径不是普通文件")
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, fileErr(err)
	}
	if cur, err := f.Stat(); err != nil || !os.SameFile(cur, info) {
		f.Close()
		return nil, nil, invalid("文件已被修改")
	}
	a, err := fileops.OpenArchive(f, filepath.Base(rel))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, a, nil
}

// ExtractVolumeFile 把卷里的压缩包解压到卷里。先检查整个压缩包（路径、文件数、大小），解压到暂存目录，
// 拿到锁以后再放进卷里。
func (m *Manager) ExtractVolumeFile(ctx context.Context, req *nagiv1.VolumeExtractRequest, prog *fileops.Progress, beforeCommit func() error) (*nagiv1.FileOpResult, error) {
	if prog == nil {
		prog = &fileops.Progress{}
	}
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	rel, err := cleanVolumePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	destDir, err := cleanVolumePath(req.GetDestDir())
	if err != nil {
		return nil, err
	}
	if req.GetIntoNewDir() {
		if destDir == "." {
			return nil, invalid("请填写目录名")
		}
		if _, err := fileops.CleanName(filepath.Base(destDir)); err != nil {
			return nil, err
		}
	}
	if err := m.precheckWrite(ctx, req.GetProjectId(), req.GetContainer()); err != nil {
		return nil, err
	}
	root, s, err := m.volumeRoot(ctx, volumeRef(req.GetProjectId(), req.GetContainer(), req.GetVolume()))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, a, err := openVolumeArchive(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	parent := destDir
	if req.GetIntoNewDir() {
		parent = filepath.Dir(destDir)
	}
	if info, err := root.Lstat(parent); err != nil || !info.IsDir() {
		return nil, invalid("目标目录不存在")
	}
	res := &nagiv1.FileOpResult{}
	if policy == fileops.ConflictAsk {
		names := a.Top
		base := destDir
		if req.GetIntoNewDir() {
			names, base = []string{filepath.Base(destDir)}, parent
		}
		for _, n := range names {
			if ok, err := fileops.Exists(root, filepath.Join(base, n)); err != nil {
				return nil, fileErr(err)
			} else if ok {
				res.Conflicts = append(res.Conflicts, n)
			}
		}
		if len(res.Conflicts) > 0 {
			return res, nil
		}
	}
	stage, cleanup, err := opStaging(s)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := metrics.CheckDisk(stage, a.Bytes); err != nil {
		return nil, err
	}
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return nil, err
	}
	defer stageRoot.Close()
	if err := stageRoot.Mkdir("x", 0o755); err != nil {
		return nil, err
	}
	uid, gid := ownerOf(root, filepath.Join(parent, "x"))
	o := &fileops.Options{UID: uid, GID: gid, Guard: &metrics.DiskGuard{Dir: stage}, Progress: prog}
	if uid >= 0 {
		_ = stageRoot.Chown("x", uid, gid)
	}
	if err := a.Extract(ctx, stageRoot, "x", o); err != nil {
		return nil, fileErr(err)
	}
	res.SkippedLinks = int32(o.Links)
	unlock, err := m.lockForWriteWait(ctx, req.GetProjectId(), req.GetContainer(), uploadLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := beforeCommit(); err != nil {
		return nil, err
	}
	place := func(srcDir, name, dst string, dir bool) error {
		final, placed, err := fileops.Place(root, dst, policy, dir, func(final string) error {
			return fileops.MoveInto(ctx, srcDir, name, root, final, &fileops.Options{UID: uid, GID: gid})
		})
		if err != nil {
			return fileErr(err)
		}
		if !placed {
			res.Skipped++
		} else {
			res.Paths = append(res.Paths, slash(final))
		}
		return nil
	}
	if req.GetIntoNewDir() {
		return res, place(stage, "x", destDir, true)
	}
	for _, top := range a.Top {
		info, err := stageRoot.Lstat(filepath.Join("x", top))
		if err != nil {
			continue // 只有被跳过的链接
		}
		if err := place(filepath.Join(stage, "x"), top, filepath.Join(destDir, top), info.IsDir()); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ListVolumeDirSizes 统计卷里一个目录下几个子目录的大小，有时间和文件数的限额（见 fileops.DirSizes）。
func (m *Manager) ListVolumeDirSizes(ctx context.Context, req *nagiv1.VolumeDirSizesRequest) (*nagiv1.DirSizesResponse, error) {
	rel, err := cleanVolumePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	names := req.GetNames()
	if len(names) > maxDirSizeNames {
		names = names[:maxDirSizeNames]
	}
	for _, n := range names {
		if _, err := fileops.CleanName(n); err != nil {
			return nil, err
		}
	}
	root, _, err := m.volumeRoot(ctx, volumeRef(req.GetProjectId(), req.GetContainer(), req.GetVolume()))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	out := &nagiv1.DirSizesResponse{}
	for _, d := range fileops.DirSizes(ctx, root.FS(), slash(rel), names, fileops.DirSizeTimeout, fileops.DirSizeMaxEntries, nil) {
		if !d.Dir {
			continue
		}
		out.Sizes = append(out.Sizes, &nagiv1.DirSize{Name: d.Name, Size: d.Size, Files: d.Files, Complete: d.Complete})
	}
	return out, nil
}

// ---- 分块上传 ----

// volumeUploads 按容器目录保存分块上传的状态（同一个上传的两块不能同时写）。
var volumeUploads sync.Map

func uploadsOf(s *nagiv1.ContainerSpec) *fileops.Uploads {
	dir := filepath.Join(containerPaths{root: s.GetDir()}.stagingDir(), "uploads")
	u, _ := volumeUploads.LoadOrStore(dir, &fileops.Uploads{Dir: dir})
	return u.(*fileops.Uploads)
}

// uploadTarget 是上传的目标：卷和卷里的路径。上传只能提交到开始时的目标。
func uploadTarget(ref *nagiv1.VolumeFileRef, rel string) string { return ref.GetVolume() + "\x00" + slash(rel) }

func uploadPath(ref *nagiv1.VolumeFileRef) (string, error) {
	rel, err := cleanVolumePath(ref.GetPath())
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", invalid("请填写文件名")
	}
	return rel, nil
}

// BeginVolumeUpload 开始或接着一个分块上传，返回已经收到的长度。部署、恢复进行中直接提示稍后重试。
func (m *Manager) BeginVolumeUpload(ctx context.Context, ref *nagiv1.VolumeFileRef, size int64, key string) (fileops.UploadInfo, error) {
	rel, err := uploadPath(ref)
	if err != nil {
		return fileops.UploadInfo{}, err
	}
	if err := m.precheckWrite(ctx, ref.GetProjectId(), ref.GetContainer()); err != nil {
		return fileops.UploadInfo{}, err
	}
	root, s, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return fileops.UploadInfo{}, err
	}
	defer root.Close()
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return fileops.UploadInfo{}, invalid("此处已有同名目录")
	}
	return uploadsOf(s).Begin(uploadTarget(ref, rel), key, size)
}

// AppendVolumeUpload 写一块。写在暂存目录里，不占容器的锁。
func (m *Manager) AppendVolumeUpload(ctx context.Context, ref *nagiv1.VolumeFileRef, id string, offset int64, sum string, r io.Reader) (int64, error) {
	rel, err := uploadPath(ref)
	if err != nil {
		return 0, err
	}
	c, err := m.Get(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return 0, err
	}
	return uploadsOf(c.Spec).Append(id, uploadTarget(ref, rel), offset, sum, r)
}

// AbortVolumeUpload 放弃一个上传，删掉已经收到的内容。
func (m *Manager) AbortVolumeUpload(ctx context.Context, ref *nagiv1.VolumeFileRef, id string) error {
	rel, err := uploadPath(ref)
	if err != nil {
		return err
	}
	c, err := m.Get(ctx, ref.GetProjectId(), ref.GetContainer())
	if err != nil {
		return err
	}
	return uploadsOf(c.Spec).Remove(id, uploadTarget(ref, rel))
}

// CommitVolumeUpload 核对整个文件的大小和 SHA-256，拿到容器的锁、确认还持有租约以后放进卷里：同一个文件系统时
// 直接改名，不再复制一遍。沿用原文件的权限和属主，新文件跟随所在目录。
func (m *Manager) CommitVolumeUpload(ctx context.Context, ref *nagiv1.VolumeFileRef, id, sum string, beforeCommit func() error) (int64, error) {
	rel, err := uploadPath(ref)
	if err != nil {
		return 0, err
	}
	unlock, err := m.lockForWriteWait(ctx, ref.GetProjectId(), ref.GetContainer(), uploadLockWait)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := beforeCommit(); err != nil {
		return 0, err
	}
	root, s, err := m.volumeRoot(ctx, ref)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	u := uploadsOf(s)
	data, size, release, err := u.Finish(id, uploadTarget(ref, rel), sum)
	if err != nil {
		return 0, err
	}
	defer release()
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return 0, invalid("此处已有同名目录")
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return 0, fileErr(err)
		}
	}
	mode, uid, gid := inheritedOwner(root, rel)
	_ = os.Chmod(data, mode)
	if uid >= 0 {
		_ = os.Chown(data, uid, gid)
	}
	err = fileops.Rename(filepath.Dir(data), filepath.Base(data), root, rel)
	if errors.Is(err, fileops.ErrCrossDevice) {
		var f *os.File
		if f, err = os.Open(data); err == nil {
			_, err = writeRootFile(root, rel, f, mode, uid, gid)
			f.Close()
		}
	}
	if err != nil {
		return 0, fileErr(err)
	}
	u.RemoveFinished(id)
	return size, nil
}
