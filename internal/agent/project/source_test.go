package project

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/go-git/go-git/v5"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// 域名解析到本机时，HTTP 和 SSH 都在连接前被拒绝，本机的服务收不到连接。
func TestGitGuardRejectsLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	opts := &git.CloneOptions{URL: "http://localhost:" + port + "/a.git"}
	guardClone(opts)
	if _, err := git.PlainClone(t.TempDir(), false, opts); err == nil || !strings.Contains(err.Error(), "已拒绝连接") {
		t.Errorf("HTTP 应该被拒绝：%v", err)
	}
	if hits.Load() != 0 {
		t.Error("本机的 HTTP 服务收到了请求")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	opts = &git.CloneOptions{
		URL:  "ssh://git@localhost:" + port + "/a.git",
		Auth: &gitssh.Password{User: "git", Password: "x", HostKeyCallbackHelper: gitssh.HostKeyCallbackHelper{HostKeyCallback: ssh.InsecureIgnoreHostKey()}},
	}
	guardClone(opts)
	if _, err := git.PlainClone(t.TempDir(), false, opts); err == nil || !strings.Contains(err.Error(), "已拒绝连接") {
		t.Errorf("SSH 应该被拒绝：%v", err)
	}
	select {
	case <-accepted:
		t.Error("本机的 SSH 端口收到了连接")
	default:
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func TestSyncDirPreserves(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	// 旧版本：代码 + 运行数据
	write(t, dst, "app.js", "old")
	write(t, dst, "removed.js", "old")
	write(t, dst, "lib/old.js", "old")
	write(t, dst, "uploads/a.png", "user data")
	write(t, dst, "var/db/data.bin", "db")
	write(t, dst, "var/cache/x", "cache")
	// 新版本
	write(t, src, "app.js", "new")
	write(t, src, "lib/new.js", "new")
	write(t, src, "uploads/placeholder", "from repo")

	var logs []string
	preserve := spec.NormalizePreserve([]string{"uploads/", "./var/db", "../evil", "/abs"})
	if err := syncDir(src, dst, preserve, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}

	checks := map[string]string{"app.js": "new", "lib/new.js": "new", "uploads/a.png": "user data", "var/db/data.bin": "db"}
	for rel, want := range checks {
		if got, ok := read(t, dst, rel); !ok || got != want {
			t.Errorf("%s = %q (exists %v), want %q", rel, got, ok, want)
		}
	}
	for _, rel := range []string{"removed.js", "lib/old.js", "var/cache/x", "uploads/placeholder"} {
		if _, ok := read(t, dst, rel); ok {
			t.Errorf("%s should not exist", rel)
		}
	}
	if len(logs) != 1 {
		t.Errorf("expected one ignore notice, got %v", logs)
	}
	if len(preserve) != 3 || preserve[2] != "abs" {
		t.Errorf("normalized preserve = %v", preserve)
	}
}

// 保留路径在节点上还不存在时，用新文件初始化；之后就以节点上的为准。
func TestSyncDirSeedsMissingPreserved(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "uploads"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "uploads", "a.txt"), []byte("seed"), 0o644)
	if err := syncDir(src, dst, []string{"uploads"}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "uploads", "a.txt")); string(b) != "seed" {
		t.Fatalf("应该用新文件初始化，实际 %q", b)
	}
	_ = os.WriteFile(filepath.Join(src, "uploads", "a.txt"), []byte("new"), 0o644)
	if err := syncDir(src, dst, []string{"uploads"}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "uploads", "a.txt")); string(b) != "seed" {
		t.Fatalf("已存在的保留路径不应该被覆盖，实际 %q", b)
	}
}
