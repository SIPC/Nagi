//go:build linux

package nodefiles

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/agent/fileops"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
)

// 覆盖同名目录时，被替换掉的旧目录按删除的规则检查：包含容器目录、Nagi 数据目录的不能覆盖，
// 根目录下的项目不能覆盖。普通目录照常覆盖。
func TestNodeOverwriteKeepsDeleteRules(t *testing.T) {
	s, dir := newService(t)
	ctx := context.Background()
	work := filepath.Join(dir, "work")
	put(t, dir+"/containers/web/compose.yml", "x")
	put(t, dir+"/data/identity/key", "secret")
	for _, name := range []string{"containers", "data", "plain"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put(t, dir+"/plain/old.txt", "old")

	for _, c := range []struct {
		name string
		req  *nagiv1.NodeTransferRequest
	}{
		{"复制覆盖包含容器目录的目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/containers"}, DestDir: dir, Copy: true, Conflict: fileops.ConflictOverwrite}},
		{"移动覆盖包含容器目录的目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/containers"}, DestDir: dir, Conflict: fileops.ConflictOverwrite}},
		{"复制覆盖 Nagi 数据目录", &nagiv1.NodeTransferRequest{Paths: []string{work + "/data"}, DestDir: dir, Copy: true, Conflict: fileops.ConflictOverwrite}},
	} {
		if _, err := s.Transfer(ctx, c.req, nil); code(err) != codes.PermissionDenied {
			t.Errorf("%s：%v，应为 PermissionDenied", c.name, err)
		}
	}
	if get(t, dir+"/containers/web/compose.yml") != "x" || get(t, dir+"/data/identity/key") != "secret" {
		t.Fatal("被拒绝的覆盖不应改动原来的内容")
	}
	// 根目录下的项目：要能在 / 下建目录（容器里以 root 运行测试时）。
	if top, err := os.MkdirTemp("/", "nagi-test-"); err == nil {
		defer os.RemoveAll(top)
		put(t, top+"/keep", "k")
		req := &nagiv1.NodeTransferRequest{Paths: []string{work + "/plain"}, DestDir: "/", NewName: filepath.Base(top), Copy: true, Conflict: fileops.ConflictOverwrite, Confirmed: true}
		if _, err := s.Transfer(ctx, req, nil); code(err) != codes.PermissionDenied {
			t.Errorf("覆盖根目录下的项目：%v，应为 PermissionDenied", err)
		}
		if get(t, top+"/keep") != "k" {
			t.Fatal("被拒绝的覆盖不应改动根目录下的项目")
		}
	}

	// 解压覆盖：压缩包顶层的 containers 目录不能替换掉包含容器目录的目录，解压到新目录时也一样。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("containers/readme.txt")
	_, _ = w.Write([]byte("hi"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	put(t, work+"/a.zip", buf.String())
	if _, err := s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/a.zip", DestDir: dir, Conflict: fileops.ConflictOverwrite}, nil); code(err) != codes.PermissionDenied {
		t.Errorf("解压覆盖包含容器目录的目录：%v，应为 PermissionDenied", err)
	}
	if _, err := s.Extract(ctx, &nagiv1.NodeExtractRequest{Path: work + "/a.zip", DestDir: dir + "/containers", IntoNewDir: true, Conflict: fileops.ConflictOverwrite}, nil); code(err) != codes.PermissionDenied {
		t.Errorf("解压到新目录并覆盖包含容器目录的目录：%v，应为 PermissionDenied", err)
	}
	if get(t, dir+"/containers/web/compose.yml") != "x" {
		t.Fatal("被拒绝的解压不应改动原来的内容")
	}

	// 普通目录照常覆盖。
	put(t, work+"/plain/new.txt", "new")
	if _, err := s.Transfer(ctx, &nagiv1.NodeTransferRequest{Paths: []string{work + "/plain"}, DestDir: dir, Copy: true, Conflict: fileops.ConflictOverwrite}, nil); err != nil {
		t.Fatalf("覆盖普通目录：%v", err)
	}
	if get(t, dir+"/plain/new.txt") != "new" {
		t.Fatal("覆盖后的内容不对")
	}
	gone(t, dir+"/plain/old.txt")
}
