// Package nodefiles 管理节点上的文件：浏览、读写、删除、备份恢复（见 docs/node-access.md#文件）。
// 路径先解析成真实路径再按规则判断。这些规则防误操作，不是安全边界：能调用这里的管理员也能打开终端。
package nodefiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/agent/metrics"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 路径的访问级别。
const (
	AccessNormal   = "normal"
	AccessConfirm  = "confirm"
	AccessReadonly = "readonly"
	AccessDenied   = "denied"
)

const (
	// MaxEdit 是能在线编辑的文件大小，也是覆盖时保留备份的上限。
	MaxEdit = 1 << 20
	// MaxUpload 是上传的单个文件的上限，和面板的一致（server/api 的 maxNodeUpload）。
	MaxUpload = 8 << 30

	maxEntries     = 5000
	chunkSize      = 256 << 10
	backupsPerFile = 10
	backupsTotal   = 200
	stagingKeep    = 24 * time.Hour
)

var systemDenied = []string{"/proc", "/sys", "/dev"}

// sensitive 是可以改、但保存前要确认的路径。
var sensitive = []string{
	"/boot", "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/sudoers", "/etc/sudoers.d",
	"/etc/ssh", "/etc/fstab", "/etc/pam.d", "/etc/systemd", "/etc/docker/daemon.json",
}

// Service 管理节点上的文件。
type Service struct {
	// Denied 是禁止访问的目录：Nagi 自己的数据目录，里面有私钥。
	Denied []string
	// Managed 返回容器目录。部署时会重写里面的文件，写入也绕过了容器的锁和租约，所以只读。
	Managed func() []string
	// BackupDir、StagingDir 是覆盖文件时的备份和上传暂存目录，都在节点目录里。
	BackupDir  string
	StagingDir string

	mu sync.Mutex // 一次只写一个文件：先比较再改名，中间不能插进别的写入。
}

func invalid(format string) error { return status.Error(codes.InvalidArgument, format) }

func denied(format string) error { return status.Error(codes.PermissionDenied, format) }

// Conflict 是保存时发现文件已经被别人改过的错误文字，面板原样返回，界面按错误码提示。
const conflictMessage = "文件已被修改"

// fsErr 把文件操作的错误转成给人看的说明。
func fsErr(err error) error {
	var pe *fs.PathError
	var le *os.LinkError
	var low *metrics.LowDiskError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &low):
		return status.Error(codes.ResourceExhausted, low.Error())
	case errors.Is(err, fs.ErrNotExist):
		return invalid("文件不存在")
	case errors.Is(err, fs.ErrExist):
		return status.Error(codes.AlreadyExists, "同名文件或目录已存在")
	case errors.Is(err, fs.ErrPermission):
		return invalid("没有访问权限")
	case errors.Is(err, syscall.ENOSPC):
		return invalid("磁盘已满，请清理空间后重试")
	case errors.Is(err, syscall.EROFS):
		return invalid("文件系统只读，无法写入")
	case errors.Is(err, syscall.ENOTEMPTY):
		return invalid("目录不为空")
	case errors.Is(err, syscall.EISDIR):
		return invalid("该路径是目录，不是文件")
	case errors.Is(err, syscall.ENOTDIR):
		return invalid("路径中有一项不是目录")
	case errors.Is(err, syscall.ENAMETOOLONG):
		return invalid("文件名过长")
	case errors.Is(err, syscall.EBUSY):
		return invalid("文件正在被使用，请稍后重试")
	case errors.Is(err, syscall.ELOOP):
		return invalid("路径中的符号链接层级过多")
	case errors.Is(err, syscall.EXDEV):
		return invalid("不能跨文件系统移动")
	case errors.As(err, &pe), errors.As(err, &le):
		return invalid("无法完成文件操作，请检查路径和权限后重试")
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// under 判断 p 是 base 或者在 base 下面。
func under(p, base string) bool {
	if base == "/" {
		return true
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

func isSensitive(p string) bool {
	if slices.ContainsFunc(sensitive, func(b string) bool { return under(p, b) }) {
		return true
	}
	return strings.HasPrefix(p, "/etc/cron") || strings.Contains(p+"/", "/.ssh/")
}

// realOf 解开符号链接，失败时返回原路径。
func realOf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

func (s *Service) deniedDirs() []string {
	out := slices.Clone(systemDenied)
	for _, d := range s.Denied {
		out = append(out, realOf(d))
	}
	return out
}

func (s *Service) managedDirs() []string {
	if s.Managed == nil {
		return nil
	}
	var out []string
	for _, d := range s.Managed() {
		if d != "" {
			out = append(out, realOf(d))
		}
	}
	return out
}

// Access 返回真实路径的访问级别。
func (s *Service) Access(real string) string {
	if slices.ContainsFunc(s.deniedDirs(), func(d string) bool { return under(real, d) }) {
		return AccessDenied
	}
	if slices.ContainsFunc(s.managedDirs(), func(d string) bool { return under(real, d) }) {
		return AccessReadonly
	}
	if isSensitive(real) {
		return AccessConfirm
	}
	return AccessNormal
}

// cleanPath 检查并整理请求里的路径：只接受绝对路径。
func cleanPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", invalid("路径须为绝对路径")
	}
	return path.Clean(p), nil
}

// resolve 把路径解析成真实路径。路径的最后一段不存在时（新建文件）只解析上级。
func resolve(p string) (string, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.ToSlash(real), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fsErr(err)
	}
	parent, err := filepath.EvalSymlinks(path.Dir(clean))
	if err != nil {
		return "", fsErr(err)
	}
	return path.Join(filepath.ToSlash(parent), path.Base(clean)), nil
}

// readable 解析路径，确认可以读。
func (s *Service) readable(p string) (string, string, error) {
	real, err := resolve(p)
	if err != nil {
		return "", "", err
	}
	a := s.Access(real)
	if a == AccessDenied {
		return "", "", denied(deniedReason(real))
	}
	return real, a, nil
}

func deniedReason(real string) string {
	if slices.ContainsFunc(systemDenied, func(d string) bool { return under(real, d) }) {
		return "禁止访问系统虚拟文件系统"
	}
	return "禁止访问 Nagi 数据目录"
}

// writable 解析路径，确认可以写。
func (s *Service) writable(p string, confirmed bool) (string, error) {
	real, a, err := s.readable(p)
	if err != nil {
		return "", err
	}
	return real, checkWrite(a, confirmed)
}

func checkWrite(a string, confirmed bool) error {
	switch a {
	case AccessReadonly:
		return denied("容器目录只读，请使用容器文件管理")
	case AccessConfirm:
		if !confirmed {
			return status.Error(codes.FailedPrecondition, "敏感文件，保存前需确认")
		}
	}
	return nil
}

// ownerNames 缓存用户名：一个目录里的文件多半是同一个属主。
type ownerNames map[string]string

func (o ownerNames) name(info fs.FileInfo) string {
	uid, gid := fileOwner(info)
	if uid < 0 {
		return ""
	}
	return o.lookup(uid) + ":" + o.lookupGroup(gid)
}

func (o ownerNames) lookup(uid int) string {
	id := strconv.Itoa(uid)
	if n, ok := o["u"+id]; ok {
		return n
	}
	n := id
	if u, err := user.LookupId(id); err == nil {
		n = u.Username
	}
	o["u"+id] = n
	return n
}

func (o ownerNames) lookupGroup(gid int) string {
	id := strconv.Itoa(gid)
	if n, ok := o["g"+id]; ok {
		return n
	}
	n := id
	if g, err := user.LookupGroupId(id); err == nil {
		n = g.Name
	}
	o["g"+id] = n
	return n
}

// List 列出目录。
func (s *Service) List(p string) (*nagiv1.ListNodeDirResponse, error) {
	real, a, err := s.readable(p)
	if err != nil {
		return nil, err
	}
	dir, err := os.Open(real)
	if err != nil {
		return nil, fsErr(err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		var pe *fs.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOTDIR) {
			return nil, invalid("该路径不是目录")
		}
		return nil, fsErr(err)
	}
	out := &nagiv1.ListNodeDirResponse{Path: real, Access: a}
	if len(entries) > maxEntries {
		entries, out.Truncated = entries[:maxEntries], true
	}
	owners := ownerNames{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		child := path.Join(real, e.Name())
		item := &nagiv1.NodeFileEntry{
			Name: e.Name(), IsDir: e.IsDir(), Size: info.Size(), Modified: timestamppb.New(info.ModTime()),
			Mode: info.Mode().Perm().String(), Owner: owners.name(info), Access: s.Access(child),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			item.IsSymlink = true
			item.LinkTarget, _ = os.Readlink(child)
			if t, err := os.Stat(child); err == nil {
				item.IsDir = t.IsDir()
			}
			item.Access = s.Access(realOf(child))
		}
		out.Entries = append(out.Entries, item)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		x, y := out.Entries[i], out.Entries[j]
		if x.GetIsDir() != y.GetIsDir() {
			return x.GetIsDir()
		}
		return x.GetName() < y.GetName()
	})
	return out, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// openRegular 打开普通文件。先看类型再打开，打开管道这类文件会卡住；O_NONBLOCK 防止这中间被换掉。
func openRegular(real string) (*os.File, fs.FileInfo, error) {
	info, err := os.Stat(real)
	if err != nil {
		return nil, nil, fsErr(err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, invalid("该路径不是普通文件")
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fsErr(err)
	}
	if info, err = f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, invalid("该路径不是普通文件")
	}
	return f, info, nil
}

// Read 读取一个文本文件的内容用于编辑。不是文本时只返回属性。
func (s *Service) Read(p string) (*nagiv1.ReadNodeFileResponse, error) {
	real, a, err := s.readable(p)
	if err != nil {
		return nil, err
	}
	f, info, err := openRegular(real)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() > MaxEdit {
		return nil, invalid("文件超过 1 MB，无法在线编辑")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxEdit+1))
	if err != nil {
		return nil, fsErr(err)
	}
	if len(b) > MaxEdit {
		return nil, invalid("文件超过 1 MB，无法在线编辑")
	}
	out := &nagiv1.ReadNodeFileResponse{
		Path: real, Sha256: hashOf(b), Modified: timestamppb.New(info.ModTime()), Mode: info.Mode().Perm().String(),
		Owner: ownerNames{}.name(info), Access: a, Size: int64(len(b)),
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		out.Binary = true
	} else {
		out.Content = b
	}
	return out, nil
}

// openParent 用 os.Root 打开真实路径的上级目录，并再次核对它还是这个目录：
// 解析路径和打开之间，路径里的某一段可能被换成了符号链接。
func openParent(real string) (*os.Root, string, error) {
	dir, base := path.Dir(real), path.Base(real)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", fsErr(err)
	}
	again, err := filepath.EvalSymlinks(dir)
	if err == nil && filepath.ToSlash(again) != dir {
		err = errors.New("changed")
	}
	if err == nil {
		var a, b fs.FileInfo
		if a, err = root.Stat("."); err == nil {
			if b, err = os.Stat(dir); err == nil && !os.SameFile(a, b) {
				err = errors.New("changed")
			}
		}
	}
	if err != nil {
		root.Close()
		return nil, "", invalid("路径已变更，请重试")
	}
	return root, base, nil
}

// writeAt 写文件：同目录写临时文件再改名，改名失败（挂载的单个文件）时原地写入。
// 目标是符号链接时替换链接本身（调用方已经解开了链接，这里不应该再遇到）。
func writeAt(root *os.Root, base string, r io.Reader, mode fs.FileMode, uid, gid int) (inPlace bool, err error) {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tmp := ".nagi-tmp-" + hex.EncodeToString(suffix)
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fsErr(err)
	}
	defer root.Remove(tmp)
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fsErr(err)
	}
	if uid >= 0 {
		_ = root.Chown(tmp, uid, gid) // 不是 root 运行（开发机）时改不了，不影响内容。
	}
	_ = root.Chmod(tmp, mode)
	err = root.Rename(tmp, base)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, syscall.EBUSY) && !errors.Is(err, syscall.EXDEV) {
		return false, fsErr(err)
	}
	src, err := root.Open(tmp)
	if err != nil {
		return false, fsErr(err)
	}
	defer src.Close()
	dst, err := root.OpenFile(base, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return false, fsErr(err)
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return true, fsErr(err)
}

// targetAttrs 决定新内容的权限和属主：覆盖时沿用原文件，新建时跟随所在目录。
func targetAttrs(root *os.Root, base string) (fs.FileInfo, fs.FileMode, int, int, error) {
	info, err := root.Lstat(base)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, 0, 0, 0, invalid("同名目录或特殊文件已存在")
		}
		uid, gid := fileOwner(info)
		return info, info.Mode().Perm(), uid, gid, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, 0, 0, fsErr(err)
	}
	uid, gid := -1, -1
	if d, err := root.Stat("."); err == nil {
		uid, gid = fileOwner(d)
	}
	return nil, 0o644, uid, gid, nil
}

// Write 保存一个文件。覆盖前保留备份。
func (s *Service) Write(req *nagiv1.WriteNodeFileRequest) (*nagiv1.WriteNodeFileResponse, error) {
	if len(req.GetContent()) > MaxEdit {
		return nil, invalid("文件超过 1 MB，请使用上传")
	}
	real, err := s.writable(req.GetPath(), req.GetConfirmed())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, base, err := openParent(real)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, mode, uid, gid, err := targetAttrs(root, base)
	if err != nil {
		return nil, err
	}
	if info != nil {
		if req.GetCreate() {
			return nil, status.Error(codes.AlreadyExists, "同名文件已存在")
		}
		cur, err := s.backup(root, base, real, info)
		if err != nil {
			return nil, err
		}
		if want := req.GetExpectedSha256(); want != "" && (cur == nil || hashOf(cur) != want) {
			return nil, status.Error(codes.Aborted, conflictMessage)
		}
		if cur != nil {
			s.saveBackup(real, cur)
		}
	} else if req.GetExpectedSha256() != "" {
		return nil, status.Error(codes.Aborted, conflictMessage)
	}
	inPlace, err := writeAt(root, base, bytes.NewReader(req.GetContent()), mode, uid, gid)
	if err != nil {
		return nil, err
	}
	return &nagiv1.WriteNodeFileResponse{Sha256: hashOf(req.GetContent()), InPlace: inPlace}, nil
}

// backup 读出要被覆盖的内容。超过 1 MB 的不备份，返回 nil。
func (s *Service) backup(root *os.Root, base, _ string, info fs.FileInfo) ([]byte, error) {
	if info.Size() > MaxEdit {
		return nil, nil
	}
	f, err := root.Open(base)
	if err != nil {
		return nil, fsErr(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fsErr(err)
	}
	return b, nil
}

func (s *Service) backupDirOf(real string) string {
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(s.BackupDir, hex.EncodeToString(sum[:8]))
}

// saveBackup 保留旧内容。失败不挡保存，只是这次没有备份。
func (s *Service) saveBackup(real string, content []byte) {
	dir := s.backupDirOf(real)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "path"), []byte(real), 0o600)
	name := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		return
	}
	s.pruneBackups()
}

type backupFile struct {
	dir, name string
}

// pruneBackups 每个文件留最近 backupsPerFile 份，整个节点留最近 backupsTotal 份。
func (s *Service) pruneBackups() {
	dirs, err := os.ReadDir(s.BackupDir)
	if err != nil {
		return
	}
	var all []backupFile
	for _, d := range dirs {
		dir := filepath.Join(s.BackupDir, d.Name())
		names := backupNames(dir)
		for i, n := range names {
			if i >= backupsPerFile {
				_ = os.Remove(filepath.Join(dir, n))
				continue
			}
			all = append(all, backupFile{dir, n})
		}
	}
	sort.Slice(all, func(i, j int) bool { return backupTime(all[i].name) > backupTime(all[j].name) })
	if len(all) > backupsTotal {
		for _, b := range all[backupsTotal:] {
			_ = os.Remove(filepath.Join(b.dir, b.name))
		}
	}
}

var backupIDRe = regexp.MustCompile(`^[0-9]+$`)

func backupTime(name string) int64 {
	n, _ := strconv.ParseInt(name, 10, 64)
	return n
}

// backupNames 返回目录里的备份，新的在前。
func backupNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if backupIDRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return backupTime(out[i]) > backupTime(out[j]) })
	return out
}

// Backups 列出一个文件的备份，新的在前。
func (s *Service) Backups(p string) (*nagiv1.ListNodeFileBackupsResponse, error) {
	real, _, err := s.readable(p)
	if err != nil {
		return nil, err
	}
	dir := s.backupDirOf(real)
	out := &nagiv1.ListNodeFileBackupsResponse{}
	for _, n := range backupNames(dir) {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		out.Backups = append(out.Backups, &nagiv1.NodeFileBackup{
			Id: n, Time: timestamppb.New(time.Unix(0, backupTime(n))), Size: info.Size(),
		})
	}
	return out, nil
}

// Restore 把文件恢复成某一份备份。恢复本身也是一次覆盖，当前内容会留一份备份。
func (s *Service) Restore(req *nagiv1.RestoreNodeFileBackupRequest) (*nagiv1.WriteNodeFileResponse, error) {
	real, err := s.writable(req.GetPath(), req.GetConfirmed())
	if err != nil {
		return nil, err
	}
	if !backupIDRe.MatchString(req.GetId()) {
		return nil, invalid("备份不存在")
	}
	b, err := os.ReadFile(filepath.Join(s.backupDirOf(real), req.GetId()))
	if err != nil {
		return nil, invalid("备份不存在")
	}
	return s.Write(&nagiv1.WriteNodeFileRequest{Path: real, Content: b, Confirmed: req.GetConfirmed()})
}

// mountPoints 返回节点上的挂载点。
func mountPoints() []string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 {
			out = append(out, unescapeMount(f[4]))
		}
	}
	return out
}

// unescapeMount 还原 mountinfo 里转义的空格等字符（\040 这样的八进制）。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Delete 删除文件或目录。符号链接只删链接本身，所以只解开上级，不解开最后一段。
func (s *Service) Delete(req *nagiv1.DeleteNodeFileRequest) error {
	clean, err := cleanPath(req.GetPath())
	if err != nil {
		return err
	}
	if path.Dir(clean) == "/" {
		return denied("不能删除根目录或一级目录")
	}
	parent, err := filepath.EvalSymlinks(path.Dir(clean))
	if err != nil {
		return fsErr(err)
	}
	real := path.Join(filepath.ToSlash(parent), path.Base(clean))
	if path.Dir(real) == "/" {
		return denied("不能删除根目录或一级目录")
	}
	info, err := os.Lstat(real)
	if err != nil {
		return fsErr(err)
	}
	a := s.Access(real)
	if a == AccessDenied {
		return denied(deniedReason(real))
	}
	if a == AccessReadonly {
		return checkWrite(a, false)
	}
	if (info.IsDir() || a == AccessConfirm) && req.GetConfirmName() != path.Base(real) {
		return status.Error(codes.FailedPrecondition, "请输入名称确认")
	}
	if info.IsDir() {
		for _, d := range s.deniedDirs() {
			if under(d, real) {
				return denied("目录包含 Nagi 数据目录，无法删除")
			}
		}
		for _, d := range s.managedDirs() {
			if under(d, real) {
				return denied("目录包含容器目录，请先删除容器")
			}
		}
		for _, m := range mountPoints() {
			if under(m, real) {
				return denied("不能删除挂载点或包含挂载点的目录")
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, base, err := openParent(real)
	if err != nil {
		return err
	}
	defer root.Close()
	return fsErr(root.RemoveAll(base))
}

// Mkdir 新建一级目录，属主跟随上级。
func (s *Service) Mkdir(p string) error {
	real, err := s.writable(p, true)
	if err != nil {
		return err
	}
	root, base, err := openParent(real)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(base, 0o755); err != nil {
		return fsErr(err)
	}
	if d, err := root.Stat("."); err == nil {
		if uid, gid := fileOwner(d); uid >= 0 {
			_ = root.Chown(base, uid, gid)
		}
	}
	return nil
}

// Download 把文件发给 send：第一条带文件名和大小，之后是内容。目录打包成 tar.gz。
func (s *Service) Download(ctx context.Context, p string, send func(*nagiv1.VolumeFileChunk) error) error {
	real, _, err := s.readable(p)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return fsErr(err)
	}
	name := path.Base(real)
	switch {
	case info.Mode().IsRegular():
		f, info, err := openRegular(real)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := send(&nagiv1.VolumeFileChunk{Name: name, Size: info.Size()}); err != nil {
			return err
		}
		return sendChunks(f, send)
	case info.IsDir():
		if real == "/" {
			return invalid("不能下载根目录")
		}
		if err := send(&nagiv1.VolumeFileChunk{Name: name + ".tar.gz", Archive: true}); err != nil {
			return err
		}
		gz := gzip.NewWriter(chunkWriter{send})
		tw := tar.NewWriter(gz)
		if err := s.tarDir(ctx, real, tw); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}
	return invalid("该路径不是普通文件")
}

func sendChunks(r io.Reader, send func(*nagiv1.VolumeFileChunk) error) error {
	buf := make([]byte, chunkSize)
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
	for off := 0; off < len(p); off += chunkSize {
		end := min(off+chunkSize, len(p))
		if err := w.send(&nagiv1.VolumeFileChunk{Data: append([]byte(nil), p[off:end]...)}); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// tarDir 把目录打进 tar。只收目录和普通文件，跳过符号链接、特殊文件和禁止访问的路径，读不了的也跳过。
func (s *Service) tarDir(ctx context.Context, real string, tw *tar.Writer) error {
	return filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		slash := filepath.ToSlash(p)
		if s.Access(slash) == AccessDenied {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		sub := strings.TrimPrefix(strings.TrimPrefix(slash, real), "/")
		if sub == "" {
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
			f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return nil
			}
			defer f.Close()
			// 边打包边有人写时，文件可能变长或变短，只取打开时的大小，不够的补零，否则 tar 会报错。
			if err := tw.WriteHeader(&tar.Header{Name: sub, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			n, err := io.CopyN(tw, f, info.Size())
			if errors.Is(err, io.EOF) {
				_, err = io.CopyN(tw, zeroReader{}, info.Size()-n)
			}
			return err
		}
		return nil
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// CheckUpload 在传输之前确认能写到这里。
func (s *Service) CheckUpload(p string, confirmed bool) error {
	real, err := s.writable(p, confirmed)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(real); err == nil && !info.Mode().IsRegular() {
		return invalid("同名目录或特殊文件已存在")
	}
	return nil
}

// Upload 把 r 写到 p，已有的文件被覆盖。传输可能很慢，先写到暂存目录，传完才写到目标位置。
func (s *Service) Upload(p string, confirmed bool, r io.Reader) (int64, error) {
	if err := s.CheckUpload(p, confirmed); err != nil {
		return 0, err
	}
	staged, n, err := s.stage(r)
	if err != nil {
		return 0, err
	}
	defer os.Remove(staged)
	real, err := s.writable(p, confirmed)
	if err != nil {
		return 0, err
	}
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
		cur, err := s.backup(root, base, real, info)
		if err != nil {
			return 0, err
		}
		if cur != nil {
			s.saveBackup(real, cur)
		}
	}
	f, err := os.Open(staged)
	if err != nil {
		return 0, fsErr(err)
	}
	defer f.Close()
	// 写到目标位置还要再占一份空间（暂存的文件写完才删）。
	if err := metrics.CheckDisk(path.Dir(real), n); err != nil {
		return 0, fsErr(err)
	}
	if _, err := writeAt(root, base, f, mode, uid, gid); err != nil {
		return 0, err
	}
	return n, nil
}

// stage 把内容写到暂存目录。失败时不留下文件；节点中途重启留下的旧文件顺便清掉。
// 超过 MaxUpload 或者磁盘剩余空间不够（metrics.DiskGuard）时停下。
func (s *Service) stage(r io.Reader) (string, int64, error) {
	if err := os.MkdirAll(s.StagingDir, 0o700); err != nil {
		return "", 0, fsErr(err)
	}
	if err := metrics.CheckDisk(s.StagingDir, 0); err != nil {
		return "", 0, fsErr(err)
	}
	if old, err := os.ReadDir(s.StagingDir); err == nil {
		for _, e := range old {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > stagingKeep {
				_ = os.Remove(filepath.Join(s.StagingDir, e.Name()))
			}
		}
	}
	f, err := os.CreateTemp(s.StagingDir, "upload-")
	if err != nil {
		return "", 0, fsErr(err)
	}
	guard := &metrics.DiskGuard{Dir: s.StagingDir}
	n, err := io.Copy(guard.Writer(f), io.LimitReader(r, MaxUpload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxUpload {
		err = invalid("单个文件不能超过 8 GB")
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fsErr(err)
	}
	return f.Name(), n, nil
}
