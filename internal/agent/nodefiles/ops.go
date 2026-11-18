package nodefiles

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/SIPC/nagi/internal/agent/fileops"
	"github.com/SIPC/nagi/internal/agent/metrics"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 节点上的改名、移动、复制、压缩、解压、目录大小和分块上传（见 docs/node-access.md#文件）。
// 规则和编辑、删除一样：禁止访问的不能碰，容器目录只读，敏感路径要确认；移进只读或禁止访问的地方也拒绝。
// 复制、打包、解压先写到目标目录里的临时名字，做完才改名成最终的名字。

// srcOf 解析要移动、复制、压缩的源：只解开上级目录，最后一段是符号链接时指的是链接本身（和删除一样）。
func (s *Service) srcOf(p string) (string, fs.FileInfo, string, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return "", nil, "", err
	}
	if clean == "/" {
		return "", nil, "", invalid("不能选择根目录")
	}
	parent, err := filepath.EvalSymlinks(path.Dir(clean))
	if err != nil {
		return "", nil, "", fsErr(err)
	}
	real := path.Join(filepath.ToSlash(parent), path.Base(clean))
	info, err := os.Lstat(real)
	if err != nil {
		return "", nil, "", fsErr(err)
	}
	a := s.Access(real)
	if a == AccessDenied {
		return "", nil, "", denied(s.deniedReason(real))
	}
	return real, info, a, nil
}

// destDirOf 解析目标目录：必须是已有的目录，能写进去。
func (s *Service) destDirOf(p string, confirmed bool) (string, error) {
	real, a, err := s.readable(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fsErr(err)
	}
	if !info.IsDir() {
		return "", invalid("目标目录不存在")
	}
	return real, checkWrite(a, confirmed)
}

// checkDest 检查要写的目标路径：禁止访问、只读的拒绝，敏感的要确认。
func (s *Service) checkDest(real string, confirmed bool) error {
	a := s.Access(real)
	if a == AccessDenied {
		return denied(s.deniedReason(real))
	}
	return checkWrite(a, confirmed)
}

// checkTree 检查要移走（或删除）的目录：不能包含 Nagi 数据目录、容器目录和挂载点。
func (s *Service) checkTree(real string) error {
	for _, d := range s.deniedDirs() {
		if under(d, real) {
			return denied("目录包含 Nagi 数据目录，无法删除、移动或覆盖")
		}
	}
	for _, d := range s.managedDirs() {
		if under(d, real) {
			return denied("目录包含容器目录，无法删除、移动或覆盖")
		}
	}
	for _, m := range mountPoints() {
		if under(m, real) {
			return denied("不能删除、移动或覆盖挂载点或包含挂载点的目录")
		}
	}
	return nil
}

// checkReplace：覆盖同名时，被替换掉的旧内容按删除的规则检查：不能覆盖一级目录（含其中的文件），
// 被替换的旧目录里不能有 Nagi 数据目录、容器目录或挂载点。目标不存在时不用检查。
func (s *Service) checkReplace(dst string) error {
	info, err := os.Lstat(dst)
	if err != nil {
		return nil
	}
	if path.Dir(dst) == "/" {
		return denied("不能覆盖根目录下的项目")
	}
	if info.IsDir() {
		return s.checkTree(dst)
	}
	return nil
}

// openDir 用 os.Root 打开真实路径上的目录，并核对它还是这个目录（见 openParent）。
func openDir(real string) (*os.Root, error) {
	root, _, err := openParent(path.Join(real, "x"))
	return root, err
}

// skipDenied 是复制、打包时跳过禁止访问的路径（parent 是源所在的目录，rel 相对它）。
func (s *Service) skipDenied(parent string) func(string) bool {
	return func(rel string) bool { return s.Access(path.Join(parent, rel)) == AccessDenied }
}

// dirOwner 是放进目录 root 的新内容的属主。
func dirOwner(root *os.Root) (int, int) {
	if d, err := root.Stat("."); err == nil {
		return fileOwner(d)
	}
	return -1, -1
}

// backupBeforeReplace 在覆盖一个 1 MB 以内的普通文件之前保留旧内容（和编辑一样）。
func (s *Service) backupBeforeReplace(root *os.Root, base, real string) {
	info, err := root.Lstat(base)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if cur, err := s.backup(root, base, real, info); err == nil && cur != nil {
		s.saveBackup(real, cur)
	}
}

type nodeItem struct {
	src, dst string
	info     fs.FileInfo
	same     bool
}

// Transfer 移动、复制或改名。conflict 为空时先查同名，有同名就只返回它们，不做改动。
func (s *Service) Transfer(ctx context.Context, req *nagiv1.NodeTransferRequest, prog *fileops.Progress) (*nagiv1.FileOpResult, error) {
	if prog == nil {
		prog = &fileops.Progress{}
	}
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	if len(req.GetPaths()) == 0 {
		return nil, invalid("请选择文件")
	}
	newName := ""
	if req.GetNewName() != "" {
		if len(req.GetPaths()) != 1 {
			return nil, invalid("只能给一项改名")
		}
		var err error
		if newName, err = fileops.CleanName(req.GetNewName()); err != nil {
			return nil, err
		}
	}
	destDir, err := s.destDirOf(req.GetDestDir(), req.GetConfirmed())
	if err != nil {
		return nil, err
	}
	res := &nagiv1.FileOpResult{}
	var items []nodeItem
	for _, p := range req.GetPaths() {
		real, info, a, err := s.srcOf(p)
		if err != nil {
			return nil, err
		}
		if !req.GetCopy() {
			if path.Dir(real) == "/" {
				return nil, denied("不能移动一级目录")
			}
			if err := checkWrite(a, req.GetConfirmed()); err != nil {
				return nil, err
			}
			if info.IsDir() {
				if err := s.checkTree(real); err != nil {
					return nil, err
				}
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			res.SkippedLinks++
			continue
		}
		name := path.Base(real)
		if newName != "" {
			name = newName
		}
		dst := path.Join(destDir, name)
		if info.IsDir() && dst != real && under(dst, real) {
			return nil, invalid("不能把目录移动或复制到它自己里面")
		}
		if dst != real && under(real, dst) && policy == fileops.ConflictOverwrite {
			return nil, invalid("不能用目录里的内容覆盖这个目录")
		}
		if err := s.checkDest(dst, req.GetConfirmed()); err != nil {
			return nil, err
		}
		if policy == fileops.ConflictOverwrite && dst != real {
			if err := s.checkReplace(dst); err != nil {
				return nil, err
			}
		}
		it := nodeItem{src: real, dst: dst, info: info, same: dst == real}
		if it.same && !req.GetCopy() {
			res.Paths = append(res.Paths, real)
			continue
		}
		items = append(items, it)
	}
	if policy == fileops.ConflictAsk {
		for _, it := range items {
			if _, err := os.Lstat(it.dst); err == nil && !it.same {
				res.Conflicts = append(res.Conflicts, path.Base(it.dst))
			}
		}
		if len(res.Conflicts) > 0 {
			return res, nil
		}
	}
	for _, it := range items {
		var final string
		var placed bool
		if req.GetCopy() {
			var links int
			final, placed, links, err = s.copyItem(ctx, it, policy, prog)
			res.SkippedLinks += int32(links)
		} else {
			final, placed, err = s.moveItem(ctx, it, policy, prog)
		}
		if err != nil {
			return res, err
		}
		if !placed {
			res.Skipped++
			continue
		}
		res.Paths = append(res.Paths, final)
	}
	return res, nil
}

// commonDir 是两个绝对路径共同的上级目录。
func commonDir(a, b string) string {
	for d := a; ; d = path.Dir(d) {
		if under(b, d) {
			return d
		}
		if d == "/" {
			return "/"
		}
	}
}

func relTo(base, p string) string {
	if base == "/" {
		return strings.TrimPrefix(p, "/")
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
}

// moveItem 在共同的上级目录里改名；跨文件系统时复制过去再删掉原来的。
func (s *Service) moveItem(ctx context.Context, it nodeItem, policy string, prog *fileops.Progress) (string, bool, error) {
	base := commonDir(path.Dir(it.src), path.Dir(it.dst))
	root, err := openDir(base)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	src, dst := filepath.FromSlash(relTo(base, it.src)), filepath.FromSlash(relTo(base, it.dst))
	s.mu.Lock()
	defer s.mu.Unlock()
	if policy == fileops.ConflictOverwrite {
		s.backupBeforeReplace(root, dst, it.dst)
	}
	final, placed, err := fileops.Place(root, dst, policy, it.info.IsDir(), func(final string) error {
		err := root.Rename(src, final)
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		uid, gid := -1, -1
		if d, err := root.Stat(filepath.Dir(final)); err == nil {
			uid, gid = fileOwner(d)
		}
		o := &fileops.Options{UID: uid, GID: gid, Guard: &metrics.DiskGuard{Dir: path.Dir(it.dst)}, Progress: prog, Skip: s.skipDenied(base)}
		tmp := fileops.TempName(filepath.Dir(final))
		if err := fileops.CopyTree(ctx, root, src, root, tmp, o); err != nil {
			_ = root.RemoveAll(tmp)
			return err
		}
		if err := root.Rename(tmp, final); err != nil {
			_ = root.RemoveAll(tmp)
			return err
		}
		return root.RemoveAll(src)
	})
	if err != nil || !placed {
		return "", placed, fsErr(err)
	}
	prog.Files.Add(1)
	return path.Join(base, filepath.ToSlash(final)), true, nil
}

// copyItem 先复制到目标目录里的临时名字，再改名成最终的名字。
func (s *Service) copyItem(ctx context.Context, it nodeItem, policy string, prog *fileops.Progress) (string, bool, int, error) {
	srcRoot, srcBase, err := openParent(it.src)
	if err != nil {
		return "", false, 0, err
	}
	defer srcRoot.Close()
	dstRoot, dstBase, err := openParent(it.dst)
	if err != nil {
		return "", false, 0, err
	}
	defer dstRoot.Close()
	uid, gid := dirOwner(dstRoot)
	o := &fileops.Options{UID: uid, GID: gid, Guard: &metrics.DiskGuard{Dir: path.Dir(it.dst)}, Progress: prog, Skip: s.skipDenied(path.Dir(it.src))}
	tmp := fileops.TempName(".")
	defer dstRoot.RemoveAll(tmp)
	if err := fileops.CopyTree(ctx, srcRoot, srcBase, dstRoot, tmp, o); err != nil {
		return "", false, 0, fsErr(err)
	}
	p := policy
	if it.same {
		p = fileops.ConflictRename
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == fileops.ConflictOverwrite {
		s.backupBeforeReplace(dstRoot, dstBase, it.dst)
	}
	final, placed, err := fileops.Place(dstRoot, dstBase, p, it.info.IsDir(), func(final string) error { return dstRoot.Rename(tmp, final) })
	if err != nil || !placed {
		return "", placed, o.Links, fsErr(err)
	}
	return path.Join(path.Dir(it.dst), final), true, o.Links, nil
}

// Compress 把同一个目录里的几项打包成 dest。禁止访问的路径、符号链接和特殊文件不打包。
func (s *Service) Compress(ctx context.Context, req *nagiv1.NodeCompressRequest, prog *fileops.Progress) (*nagiv1.FileOpResult, error) {
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	if req.GetFormat() != fileops.FormatZip && req.GetFormat() != fileops.FormatTarGz {
		return nil, invalid("压缩格式只能是 zip 或 tar.gz")
	}
	if len(req.GetPaths()) == 0 {
		return nil, invalid("请选择文件")
	}
	parent := ""
	var names []string
	for _, p := range req.GetPaths() {
		real, _, _, err := s.srcOf(p)
		if err != nil {
			return nil, err
		}
		if parent != "" && path.Dir(real) != parent {
			return nil, invalid("只能压缩同一目录里的文件")
		}
		parent = path.Dir(real)
		names = append(names, path.Base(real))
	}
	dest, err := s.writable(req.GetDest(), req.GetConfirmed())
	if err != nil {
		return nil, err
	}
	if _, err := fileops.CleanName(path.Base(dest)); err != nil {
		return nil, err
	}
	res := &nagiv1.FileOpResult{}
	if info, err := os.Lstat(dest); err == nil {
		if info.IsDir() {
			return nil, invalid("同名目录或特殊文件已存在")
		}
		if policy == fileops.ConflictAsk {
			res.Conflicts = []string{path.Base(dest)}
			return res, nil
		}
	}
	srcRoot, err := openDir(parent)
	if err != nil {
		return nil, err
	}
	defer srcRoot.Close()
	dstRoot, dstBase, err := openParent(dest)
	if err != nil {
		return nil, err
	}
	defer dstRoot.Close()
	tmp := fileops.TempName(".")
	defer dstRoot.RemoveAll(tmp)
	out, err := dstRoot.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fsErr(err)
	}
	o := &fileops.Options{UID: -1, GID: -1, Guard: &metrics.DiskGuard{Dir: path.Dir(dest)}, Progress: prog, Skip: s.skipDenied(parent)}
	err = fileops.WriteArchive(ctx, srcRoot, names, req.GetFormat(), out, o)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fsErr(err)
	}
	res.SkippedLinks = int32(o.Links)
	if uid, gid := dirOwner(dstRoot); uid >= 0 {
		_ = dstRoot.Chown(tmp, uid, gid)
	}
	_ = dstRoot.Chmod(tmp, 0o644)
	s.mu.Lock()
	defer s.mu.Unlock()
	if policy == fileops.ConflictOverwrite {
		s.backupBeforeReplace(dstRoot, dstBase, dest)
	}
	final, placed, err := fileops.Place(dstRoot, dstBase, policy, false, func(final string) error { return dstRoot.Rename(tmp, final) })
	if err != nil {
		return nil, fsErr(err)
	}
	if !placed {
		res.Skipped++
	} else {
		res.Paths = []string{path.Join(path.Dir(dest), final)}
	}
	return res, nil
}

// Extract 解压节点上的压缩包。先检查整个压缩包（路径、文件数、大小），解压到目标目录里的临时目录，
// 做完再改名成最终的名字。
func (s *Service) Extract(ctx context.Context, req *nagiv1.NodeExtractRequest, prog *fileops.Progress) (*nagiv1.FileOpResult, error) {
	policy := req.GetConflict()
	if !fileops.ValidConflict(policy) {
		return nil, invalid("同名处理方式无效")
	}
	real, _, err := s.readable(req.GetPath())
	if err != nil {
		return nil, err
	}
	f, _, err := openRegular(real)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := fileops.OpenArchive(f, path.Base(real))
	if err != nil {
		return nil, err
	}
	var dest, parent string
	if req.GetIntoNewDir() {
		if dest, err = s.writable(req.GetDestDir(), req.GetConfirmed()); err != nil {
			return nil, err
		}
		if _, err := fileops.CleanName(path.Base(dest)); err != nil {
			return nil, err
		}
		parent = path.Dir(dest)
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			return nil, invalid("目标目录不存在")
		}
		if policy == fileops.ConflictOverwrite {
			if err := s.checkReplace(dest); err != nil {
				return nil, err
			}
		}
	} else {
		if dest, err = s.destDirOf(req.GetDestDir(), req.GetConfirmed()); err != nil {
			return nil, err
		}
		parent = dest
		for _, top := range a.Top {
			if err := s.checkDest(path.Join(dest, top), req.GetConfirmed()); err != nil {
				return nil, err
			}
			if policy == fileops.ConflictOverwrite {
				if err := s.checkReplace(path.Join(dest, top)); err != nil {
					return nil, err
				}
			}
		}
	}
	res := &nagiv1.FileOpResult{}
	if policy == fileops.ConflictAsk {
		names, base := a.Top, dest
		if req.GetIntoNewDir() {
			names, base = []string{path.Base(dest)}, parent
		}
		for _, n := range names {
			if _, err := os.Lstat(path.Join(base, n)); err == nil {
				res.Conflicts = append(res.Conflicts, n)
			}
		}
		if len(res.Conflicts) > 0 {
			return res, nil
		}
	}
	if err := metrics.CheckDisk(parent, a.Bytes); err != nil {
		return nil, fsErr(err)
	}
	root, err := openDir(parent)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	uid, gid := dirOwner(root)
	tmp := fileops.TempName(".")
	if err := root.Mkdir(tmp, 0o755); err != nil {
		return nil, fsErr(err)
	}
	defer root.RemoveAll(tmp)
	if uid >= 0 {
		_ = root.Chown(tmp, uid, gid)
	}
	o := &fileops.Options{UID: uid, GID: gid, Guard: &metrics.DiskGuard{Dir: parent}, Progress: prog}
	if err := a.Extract(ctx, root, tmp, o); err != nil {
		return nil, fsErr(err)
	}
	res.SkippedLinks = int32(o.Links)
	s.mu.Lock()
	defer s.mu.Unlock()
	place := func(from, to string, dir bool) error {
		if policy == fileops.ConflictOverwrite {
			s.backupBeforeReplace(root, to, path.Join(parent, filepath.ToSlash(to)))
		}
		final, placed, err := fileops.Place(root, to, policy, dir, func(final string) error { return root.Rename(from, final) })
		if err != nil {
			return fsErr(err)
		}
		if !placed {
			res.Skipped++
		} else {
			res.Paths = append(res.Paths, path.Join(parent, filepath.ToSlash(final)))
		}
		return nil
	}
	if req.GetIntoNewDir() {
		return res, place(tmp, path.Base(dest), true)
	}
	for _, top := range a.Top {
		info, err := root.Lstat(filepath.Join(tmp, top))
		if err != nil {
			continue // 只有被跳过的链接
		}
		if err := place(filepath.Join(tmp, top), top, info.IsDir()); err != nil {
			return res, err
		}
	}
	return res, nil
}

// DirSizes 统计目录下几个子目录的大小，跳过禁止访问的路径，有时间和文件数的限额。
func (s *Service) DirSizes(ctx context.Context, req *nagiv1.NodeDirSizesRequest) (*nagiv1.DirSizesResponse, error) {
	real, _, err := s.readable(req.GetPath())
	if err != nil {
		return nil, err
	}
	names := req.GetNames()
	if len(names) > maxEntries {
		names = names[:maxEntries]
	}
	var keep []string
	for _, n := range names {
		if _, err := fileops.CleanName(n); err != nil {
			return nil, err
		}
		if s.Access(path.Join(real, n)) != AccessDenied {
			keep = append(keep, n)
		}
	}
	skip := func(p string) bool { return s.Access(path.Join(real, p)) == AccessDenied }
	timeout, maxEntries, release, err := fileops.DirSizeLimits(ctx, req.GetFull())
	if err != nil {
		return nil, err
	}
	defer release()
	out := &nagiv1.DirSizesResponse{}
	for _, d := range fileops.DirSizes(ctx, os.DirFS(real), ".", keep, timeout, maxEntries, req.GetMaxBytes(), skip) {
		if d.Dir {
			out.Sizes = append(out.Sizes, &nagiv1.DirSize{Name: d.Name, Size: d.Size, Files: d.Files, Complete: d.Complete, TooLarge: d.TooLarge})
		}
	}
	return out, nil
}

// ---- 分块上传 ----

var chunkUploads sync.Map

func (s *Service) chunks() *fileops.Uploads {
	dir := filepath.Join(s.StagingDir, "uploads")
	u, _ := chunkUploads.LoadOrStore(dir, &fileops.Uploads{Dir: dir})
	return u.(*fileops.Uploads)
}

// BeginUpload 开始或接着一个分块上传。目标先按上传的规则检查一遍。
func (s *Service) BeginUpload(p string, confirmed bool, size int64, key string) (fileops.UploadInfo, error) {
	if err := s.CheckUpload(p, confirmed); err != nil {
		return fileops.UploadInfo{}, err
	}
	real, err := resolve(p)
	if err != nil {
		return fileops.UploadInfo{}, err
	}
	info, err := s.chunks().Begin(real, key, size)
	return info, fsErr(err)
}

// AppendUpload 写一块到暂存目录。
func (s *Service) AppendUpload(p, id string, offset int64, sum string, r io.Reader) (int64, error) {
	real, err := resolve(p)
	if err != nil {
		return 0, err
	}
	n, err := s.chunks().Append(id, real, offset, sum, r)
	return n, fsErr(err)
}

// AbortUpload 放弃一个上传。
func (s *Service) AbortUpload(p, id string) error {
	real, err := resolve(p)
	if err != nil {
		return err
	}
	return fsErr(s.chunks().Remove(id, real))
}

// CommitUpload 核对整个文件以后写到目标位置：同一个文件系统时直接改名，否则复制（挂载的单个文件原地写入）。
// 沿用原文件的权限和属主，新文件跟随所在目录；覆盖 1 MB 以内的文件前保留旧内容。
func (s *Service) CommitUpload(p string, confirmed bool, id, sum string) (int64, error) {
	real, err := s.writable(p, confirmed)
	if err != nil {
		return 0, err
	}
	u := s.chunks()
	data, size, release, err := u.Finish(id, real, sum)
	if err != nil {
		return 0, fsErr(err)
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	root, base, err := openParent(real)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	info, mode, uid, gid, err := targetAttrs(root, base)
	if err != nil {
		return 0, err
	}
	if info != nil {
		s.backupBeforeReplace(root, base, real)
	}
	_ = os.Chmod(data, mode)
	if uid >= 0 {
		_ = os.Chown(data, uid, gid)
	}
	err = fileops.Rename(filepath.Dir(data), filepath.Base(data), root, base)
	if errors.Is(err, fileops.ErrCrossDevice) || errors.Is(err, syscall.EBUSY) {
		if err = metrics.CheckDisk(path.Dir(real), size); err != nil {
			return 0, fsErr(err)
		}
		var f *os.File
		if f, err = os.Open(data); err == nil {
			_, err = writeAt(root, base, f, mode, uid, gid)
			f.Close()
		}
	}
	if err != nil {
		return 0, fsErr(err)
	}
	u.RemoveFinished(id)
	return size, nil
}
