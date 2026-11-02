//go:build linux

package nodefiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newService 建一个测试用的环境：dir/data 禁止访问，dir/containers/web 是容器目录。
func newService(t *testing.T) (*Service, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"data/identity", "containers/web", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{
		Denied:     []string{filepath.Join(dir, "data")},
		Managed:    func() []string { return []string{filepath.Join(dir, "containers/web")} },
		BackupDir:  filepath.Join(dir, "backups"),
		StagingDir: filepath.Join(dir, "staging"),
	}, dir
}

func code(err error) codes.Code { return status.Code(err) }

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAccess(t *testing.T) {
	s, dir := newService(t)
	cases := map[string]string{
		"/proc/1/environ":              AccessDenied,
		"/sys":                         AccessDenied,
		"/device":                      AccessNormal,
		dir + "/data/identity/key":     AccessDenied,
		dir + "/containers/web/data/a": AccessReadonly,
		dir + "/containers/web2":       AccessNormal,
		"/etc/shadow":                  AccessConfirm,
		"/etc/sudoers.d/x":             AccessConfirm,
		"/etc/cron.daily/x":            AccessConfirm,
		"/root/.ssh/authorized_keys":   AccessConfirm,
		"/home/a/.ssh":                 AccessConfirm,
		"/etc/hostname":                AccessNormal,
		"/opt/app/config.yml":          AccessNormal,
	}
	for p, want := range cases {
		if got := s.Access(p); got != want {
			t.Errorf("%s：%s，应该是 %s", p, got, want)
		}
	}
}

func TestDeniedThroughSymlink(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/data/identity/key", "secret")
	if err := os.Symlink(dir+"/data/identity", dir+"/work/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(dir + "/work/link/key"); code(err) != codes.PermissionDenied {
		t.Fatalf("经符号链接读私钥应该被拒绝：%v", err)
	}
	if _, err := s.List(dir + "/work/link"); code(err) != codes.PermissionDenied {
		t.Fatalf("经符号链接列目录应该被拒绝：%v", err)
	}
	_, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/link/new", Content: []byte("x")})
	if code(err) != codes.PermissionDenied {
		t.Fatalf("经符号链接写应该被拒绝：%v", err)
	}
	res, err := s.List(dir + "/work")
	if err != nil {
		t.Fatal(err)
	}
	if e := res.GetEntries()[0]; e.GetName() != "link" || !e.GetIsSymlink() || e.GetAccess() != AccessDenied || e.GetLinkTarget() != dir+"/data/identity" {
		t.Fatalf("链接的属性不对：%v", e)
	}
	// 容器目录也一样，经链接也只读。
	if err := os.Symlink(dir+"/containers/web", dir+"/work/c"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/c/x", Content: []byte("x")})
	if code(err) != codes.PermissionDenied {
		t.Fatalf("经符号链接写容器目录应该被拒绝：%v", err)
	}
}

func TestRelativeAndEmptyPath(t *testing.T) {
	s, _ := newService(t)
	for _, p := range []string{"", "etc/passwd", "../etc", "/etc/\x00x"} {
		if _, err := s.List(p); code(err) != codes.InvalidArgument {
			t.Errorf("%q 应该被拒绝：%v", p, err)
		}
	}
}

func TestWriteConflictBackupRestore(t *testing.T) {
	s, dir := newService(t)
	p := dir + "/work/a.conf"
	write(t, p, "one")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur.GetContent()) != "one" {
		t.Fatalf("内容 %q", cur.GetContent())
	}
	// 文件被别人改了，用旧的哈希保存要失败。
	write(t, p, "two")
	_, err = s.Write(&nagiv1.WriteNodeFileRequest{Path: p, Content: []byte("mine"), ExpectedSha256: cur.GetSha256()})
	if code(err) != codes.Aborted {
		t.Fatalf("应该发现冲突：%v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("冲突时不能写：%q", b)
	}
	if b, _ := s.Backups(p); len(b.GetBackups()) != 0 {
		t.Fatalf("冲突时不该留备份：%v", b)
	}
	// 用户确认覆盖。
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: p, Content: []byte("mine")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("要沿用原来的权限：%v", info.Mode())
	}
	backups, err := s.Backups(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups.GetBackups()) != 1 || backups.GetBackups()[0].GetSize() != 3 {
		t.Fatalf("备份：%v", backups)
	}
	if _, err := s.Restore(&nagiv1.RestoreNodeFileBackupRequest{Path: p, Id: backups.GetBackups()[0].GetId()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("恢复后：%q", b)
	}
	if _, err := s.Restore(&nagiv1.RestoreNodeFileBackupRequest{Path: p, Id: "../../x"}); code(err) != codes.InvalidArgument {
		t.Fatalf("备份编号要校验：%v", err)
	}
	if entries, _ := os.ReadDir(dir + "/work"); len(entries) != 1 {
		t.Fatalf("不能留下临时文件：%v", entries)
	}
}

func TestBackupLimit(t *testing.T) {
	s, dir := newService(t)
	p := dir + "/work/a"
	write(t, p, "0")
	for i := 0; i < backupsPerFile+3; i++ {
		if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: p, Content: []byte(strings.Repeat("x", i+1))}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := s.Backups(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.GetBackups()) != backupsPerFile {
		t.Fatalf("保留了 %d 份", len(b.GetBackups()))
	}
}

func TestWriteRules(t *testing.T) {
	s, dir := newService(t)
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/containers/web/x", Content: []byte("x")}); code(err) != codes.PermissionDenied {
		t.Fatalf("容器目录只读：%v", err)
	}
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/data/x", Content: []byte("x")}); code(err) != codes.PermissionDenied {
		t.Fatalf("数据目录禁止：%v", err)
	}
	if err := os.MkdirAll(dir+"/work/.ssh", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/.ssh/x", Content: []byte("x")}); code(err) != codes.FailedPrecondition {
		t.Fatalf("敏感文件要确认：%v", err)
	}
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/big", Content: make([]byte, MaxEdit+1)}); code(err) != codes.InvalidArgument {
		t.Fatalf("超过上限：%v", err)
	}
	write(t, dir+"/work/old", "x")
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/old", Content: []byte("y"), Create: true}); code(err) != codes.AlreadyExists {
		t.Fatalf("新建不能覆盖：%v", err)
	}
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work", Content: []byte("y")}); code(err) != codes.InvalidArgument {
		t.Fatalf("不能写目录：%v", err)
	}
}

func TestWriteThroughSymlinkKeepsLink(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/work/real", "one")
	if err := os.Symlink("real", dir+"/work/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(&nagiv1.WriteNodeFileRequest{Path: dir + "/work/link", Content: []byte("two")}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(dir + "/work/link"); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("链接不能被换成普通文件")
	}
	if b, _ := os.ReadFile(dir + "/work/real"); string(b) != "two" {
		t.Fatalf("目标文件：%q", b)
	}
}

func TestReadSpecialAndBinary(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/work/bin", "a\x00b")
	r, err := s.Read(dir + "/work/bin")
	if err != nil || !r.GetBinary() || r.GetContent() != nil {
		t.Fatalf("二进制文件：%v %v", r, err)
	}
	if _, err := s.Read(dir + "/work"); code(err) != codes.InvalidArgument {
		t.Fatalf("目录不能当文件读：%v", err)
	}
	if err := os.WriteFile(dir+"/work/big", make([]byte, MaxEdit+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(dir + "/work/big"); code(err) != codes.InvalidArgument {
		t.Fatalf("大文件不能在线编辑：%v", err)
	}
}

func TestDelete(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/work/f", "x")
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/work/f"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/work/d/e", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/work/d"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("删目录要确认：%v", err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/work/d", ConfirmName: "d"}); err != nil {
		t.Fatal(err)
	}
	// 符号链接只删链接。
	write(t, dir+"/work/target", "x")
	if err := os.Symlink(dir+"/work/target", dir+"/work/l"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/work/l"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + "/work/target"); err != nil {
		t.Fatal("目标不能被删")
	}
	for _, p := range []string{"/", "/etc", "/usr"} {
		if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: p, ConfirmName: filepath.Base(p)}); code(err) != codes.PermissionDenied {
			t.Errorf("%s 不能删：%v", p, err)
		}
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/containers/web", ConfirmName: "web"}); code(err) != codes.PermissionDenied {
		t.Fatalf("容器目录不能删：%v", err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/containers", ConfirmName: "containers"}); code(err) != codes.PermissionDenied {
		t.Fatalf("包含容器目录的目录不能删：%v", err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir + "/data", ConfirmName: "data"}); code(err) != codes.PermissionDenied {
		t.Fatalf("数据目录不能删：%v", err)
	}
	if err := s.Delete(&nagiv1.DeleteNodeFileRequest{Path: dir, ConfirmName: filepath.Base(dir)}); code(err) != codes.PermissionDenied {
		t.Fatalf("包含数据目录的目录不能删：%v", err)
	}
}

func TestMkdirAndUpload(t *testing.T) {
	s, dir := newService(t)
	if err := s.Mkdir(dir + "/work/new"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(dir + "/work/new"); code(err) != codes.AlreadyExists {
		t.Fatalf("已存在：%v", err)
	}
	if err := s.Mkdir(dir + "/containers/web/x"); code(err) != codes.PermissionDenied {
		t.Fatalf("容器目录只读：%v", err)
	}
	n, err := s.Upload(dir+"/work/new/up", false, strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if _, err := s.Upload(dir+"/work/new/up", false, strings.NewReader("world")); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Backups(dir + "/work/new/up"); len(b.GetBackups()) != 1 {
		t.Fatalf("上传覆盖要留备份：%v", b)
	}
	if _, err := s.Upload(dir+"/work/new", false, strings.NewReader("x")); code(err) != codes.InvalidArgument {
		t.Fatalf("不能覆盖目录：%v", err)
	}
	if entries, _ := os.ReadDir(s.StagingDir); len(entries) != 0 {
		t.Fatalf("暂存文件要清掉：%v", entries)
	}
}

func TestDownloadDir(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/work/a", "aaa")
	if err := os.MkdirAll(dir+"/work/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir+"/work/sub/b", "bb")
	if err := os.Symlink("/etc/passwd", dir+"/work/link"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	head := false
	err := s.Download(context.Background(), dir+"/work", func(c *nagiv1.VolumeFileChunk) error {
		if !head {
			head = true
			if !c.GetArchive() || c.GetName() != "work.tar.gz" {
				t.Fatalf("第一条：%v", c)
			}
			return nil
		}
		buf.Write(c.GetData())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "a,sub/,sub/b" {
		t.Fatalf("打包内容：%v", names)
	}
	if err := s.Download(context.Background(), "/", func(*nagiv1.VolumeFileChunk) error { return nil }); code(err) != codes.InvalidArgument {
		t.Fatalf("根目录不能打包：%v", err)
	}
	if err := s.Download(context.Background(), dir+"/data", func(*nagiv1.VolumeFileChunk) error { return nil }); code(err) != codes.PermissionDenied {
		t.Fatalf("数据目录不能下载：%v", err)
	}
}

func TestDownloadSkipsDenied(t *testing.T) {
	s, dir := newService(t)
	write(t, dir+"/data/identity/key", "secret")
	write(t, dir+"/work/a", "a")
	var buf bytes.Buffer
	first := true
	err := s.Download(context.Background(), dir, func(c *nagiv1.VolumeFileChunk) error {
		if !first {
			buf.Write(c.GetData())
		}
		first = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(h.Name, "data") {
			t.Fatalf("打包了禁止访问的内容：%s", h.Name)
		}
	}
}
