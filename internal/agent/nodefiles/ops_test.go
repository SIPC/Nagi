//go:build linux

package nodefiles

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/agent/fileops"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func put(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err == nil {
		t.Fatalf("%s 不应该存在", p)
	}
}

func TestNodeTransfer(t *testing.T) {
	s, dir := newService(t)
	ctx := context.Background()
	work := filepath.Join(dir, "work")
	put(t, filepath.Join(work, "a.txt"), "a")
	put(t, filepath.Join(work, "src", "b.txt"), "b")
	if err := os.Mkdir(filepath.Join(work, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 改名
	res, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/a.txt"}, DestDir: work, NewName: "c.txt"}, nil)
	if err != nil || !slices.Equal(res.GetPaths(), []string{work + "/c.txt"}) {
		t.Fatalf("改名：%v %v", res, err)
	}
	// 移动
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/c.txt", work + "/src"}, DestDir: work + "/dst"}, nil); err != nil {
		t.Fatal(err)
	}
	if get(t, work+"/dst/c.txt") != "a" || get(t, work+"/dst/src/b.txt") != "b" {
		t.Fatal("移动后的内容不对")
	}
	// 复制到原处保留两者
	res, err = s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/dst/src"}, DestDir: work + "/dst", Copy: true}, nil)
	if err != nil || !slices.Equal(res.GetPaths(), []string{work + "/dst/src (2)"}) || get(t, work+"/dst/src (2)/b.txt") != "b" {
		t.Fatalf("复制：%v %v", res, err)
	}
	// 同名先问，覆盖前保留备份
	put(t, work+"/c.txt", "new")
	res, err = s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/c.txt"}, DestDir: work + "/dst", Copy: true}, nil)
	if err != nil || !slices.Equal(res.GetConflicts(), []string{"c.txt"}) {
		t.Fatalf("同名应该先问：%v %v", res, err)
	}
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/c.txt"}, DestDir: work + "/dst", Copy: true, Conflict: fileops.ConflictOverwrite}, nil); err != nil {
		t.Fatal(err)
	}
	if get(t, work+"/dst/c.txt") != "new" {
		t.Fatal("覆盖后的内容不对")
	}
	if b, _ := s.Backups(work + "/dst/c.txt"); len(b.GetBackups()) != 1 {
		t.Fatal("覆盖前应该保留旧内容")
	}
	// 目录不能移进自己里面
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/dst"}, DestDir: work + "/dst/src"}, nil); err == nil {
		t.Fatal("目录不能移进自己里面")
	}
}

func TestNodeTransferRules(t *testing.T) {
	s, dir := newService(t)
	ctx := context.Background()
	work := filepath.Join(dir, "work")
	put(t, work+"/f", "f")
	put(t, dir+"/containers/web/compose.yml", "x")
	cases := []struct {
		name string
		req  *nagiv1.NodeTransferRequest
		code codes.Code
	}{
		{"移进禁止访问的目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/f"}, DestDir: dir + "/data"}, codes.PermissionDenied},
		{"复制进禁止访问的目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/f"}, DestDir: dir + "/data/identity", Copy: true}, codes.PermissionDenied},
		{"移进容器目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/f"}, DestDir: dir + "/containers/web"}, codes.PermissionDenied},
		{"从容器目录移走", &nagiv1.NodeTransferRequest{Paths: []string{dir + "/containers/web/compose.yml"}, DestDir: work}, codes.PermissionDenied},
		{"移走禁止访问的目录", &nagiv1.NodeTransferRequest{Paths: []string{dir + "/data"}, DestDir: work}, codes.PermissionDenied},
		{"改名成敏感路径没有确认", &nagiv1.NodeTransferRequest{Paths: []string{work + "/f"}, DestDir: work, NewName: ".ssh"}, codes.FailedPrecondition},
		{"移动一级目录", &nagiv1.NodeTransferRequest{Paths: []string{"/tmp"}, DestDir: work}, codes.PermissionDenied},
	}
	for _, c := range cases {
		if _, err := s.Transfer(ctx, c.req, nil); status.Code(err) != c.code {
			t.Errorf("%s：%v，应为 %v", c.name, err, c.code)
		}
	}
	// 从容器目录复制出来可以（只读）。
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{dir + "/containers/web/compose.yml"}, DestDir: work, Copy: true}, nil); err != nil {
		t.Fatalf("从只读目录复制应该可以：%v", err)
	}
	// 确认以后可以写敏感路径。
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/f"}, DestDir: work, NewName: ".ssh", Confirmed: true}, nil); err != nil {
		t.Fatalf("确认以后应该可以：%v", err)
	}
}

func TestNodeCopySkipsDeniedAndLinks(t *testing.T) {
	s, dir := newService(t)
	ctx := context.Background()
	work := filepath.Join(dir, "work")
	put(t, dir+"/data/identity/key", "secret")
	put(t, work+"/src/a", "a")
	if err := os.Symlink(dir+"/data/identity/key", work+"/src/key"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/data", work+"/src/datalink"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/src"}, DestDir: work, NewName: "copy", Copy: true}, nil)
	if err != nil || res.GetSkippedLinks() != 2 {
		t.Fatalf("应该跳过两个链接：%v %v", res, err)
	}
	gone(t, work+"/copy/key")
	gone(t, work+"/copy/datalink")
	// 打包也一样。
	res, err = s.Compress(ctx, &nagiv1.NodeCompressRequest{Paths: []string{work + "/src"}, Dest: work + "/src.zip", Format: fileops.FormatZip}, nil)
	if err != nil || res.GetSkippedLinks() != 2 {
		t.Fatalf("打包应该跳过链接：%v %v", res, err)
	}
	zr, err := zip.OpenReader(work + "/src.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.Contains(f.Name, "key") || strings.Contains(f.Name, "datalink") {
			t.Fatalf("打包了 %s", f.Name)
		}
	}
}

func TestNodeCompressExtract(t *testing.T) {
	s, dir := newService(t)
	ctx := context.Background()
	work := filepath.Join(dir, "work")
	put(t, work+"/site/index.html", "hi")
	put(t, work+"/site/a/b.css", "css")
	if _, err := s.Compress(ctx, &nagiv1.NodeCompressRequest{Paths: []string{work + "/site"}, Dest: work + "/site.tar.gz", Format: fileops.FormatTarGz}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/site.tar.gz", DestDir: work + "/site", IntoNewDir: true}, nil)
	if err != nil || !slices.Equal(res.GetConflicts(), []string{"site"}) {
		t.Fatalf("同名目录应该先问：%v %v", res, err)
	}
	res, err = s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/site.tar.gz", DestDir: work + "/site", IntoNewDir: true, Conflict: fileops.ConflictRename}, nil)
	if err != nil || get(t, work+"/site (2)/site/a/b.css") != "css" {
		t.Fatalf("解压：%v %v", res, err)
	}
	// 解压进容器目录：拒绝。
	if _, err := s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/site.tar.gz", DestDir: dir + "/containers/web"}, nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("容器目录只读：%v", err)
	}
	// 不安全的路径：拒绝，什么都不写。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"ok", "../../data/identity/evil"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	put(t, work+"/slip.zip", buf.String())
	if _, err := s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/slip.zip", DestDir: work}, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("应该拒绝：%v", err)
	}
	gone(t, work+"/ok")
	gone(t, dir+"/data/identity/evil")
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nagi-tmp-") {
			t.Fatalf("留下了临时目录 %s", e.Name())
		}
	}
}

func TestNodeDirSizes(t *testing.T) {
	s, dir := newService(t)
	put(t, dir+"/work/a/1", "12345")
	put(t, dir+"/data/identity/key", "secret")
	res, err := s.DirSizes(context.Background(), &nagiv1.NodeDirSizesRequest{Path: dir, Names: []string{"work", "data"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetSizes()) != 1 || res.GetSizes()[0].GetName() != "work" || res.GetSizes()[0].GetSize() != 5 {
		t.Fatalf("禁止访问的目录不统计：%v", res)
	}
}

func TestNodeChunkedUpload(t *testing.T) {
	s, dir := newService(t)
	target := dir + "/work/up.bin"
	put(t, target, "old")
	content := bytes.Repeat([]byte("x"), 100000)
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	info, err := s.BeginUpload(target, false, int64(len(content)), "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpload(target, info.ID, 0, sum(content[:60000]), bytes.NewReader(content[:60000])); err != nil {
		t.Fatal(err)
	}
	again, err := s.BeginUpload(target, false, int64(len(content)), "k")
	if err != nil || again.Offset != 60000 {
		t.Fatalf("应该接着传：%+v %v", again, err)
	}
	if _, err := s.AppendUpload(target, info.ID, 60000, sum(content[60000:]), bytes.NewReader(content[60000:])); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUpload(target, false, info.ID, sum(content)); err != nil {
		t.Fatal(err)
	}
	if get(t, target) != string(content) {
		t.Fatal("内容不对")
	}
	if b, _ := s.Backups(target); len(b.GetBackups()) != 1 {
		t.Fatal("覆盖前应该保留旧内容")
	}
	if _, err := s.BeginUpload(dir+"/containers/web/x", false, 1, "k"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("容器目录只读：%v", err)
	}
	if _, err := s.BeginUpload(dir+"/data/x", false, 1, "k"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("禁止访问：%v", err)
	}
}
