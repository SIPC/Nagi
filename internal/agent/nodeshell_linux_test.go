//go:build linux

package agent

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// readUntil 读 shell 的输出，直到出现 want。
func readUntil(t *testing.T, s *shellSession, want string) string {
	t.Helper()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := s.Read(buf)
			out.Write(buf[:n])
			if err != nil || strings.Contains(out.String(), want) {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("等不到 %q，输出：%q", want, out.String())
	}
	return out.String()
}

func TestShellEnvAndCwd(t *testing.T) {
	t.Setenv("NAGI_SECRET_TEST", "leak")
	dir := t.TempDir()
	s, err := startShell(dir, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("printf 'cwd=%s secret=[%s] mark=%s size=%s\\n' \"$PWD\" \"$NAGI_SECRET_TEST\" \"$NAGI_NODE_SHELL\" \"$(stty size)\"; echo DO''NE\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "DONE")
	if !strings.Contains(out, "cwd="+dir) {
		t.Errorf("起始目录不对：%q", out)
	}
	if !strings.Contains(out, "secret=[]") {
		t.Errorf("不该继承节点进程的环境变量：%q", out)
	}
	if !strings.Contains(out, "mark=1") {
		t.Errorf("没有 NAGI_NODE_SHELL：%q", out)
	}
	if !strings.Contains(out, "size=30 100") {
		t.Errorf("窗口大小不对：%q", out)
	}
}

func TestShellBadCwdFallsBackToHome(t *testing.T) {
	s, err := startShell("/no/such/dir", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("pwd; echo DO''NE\n")); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if out := readUntil(t, s, "DONE"); !strings.Contains(out, home) {
		t.Errorf("应该回到主目录 %s：%q", home, out)
	}
}

func TestShellExitCode(t *testing.T) {
	s, err := startShell("", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	for {
		if _, err := s.Read(buf); err != nil {
			break
		}
	}
	if code := s.Wait(); code != 7 {
		t.Fatalf("退出码 %d", code)
	}
}

// 连接断开时，shell 里正在跑的前台命令也要结束。
func TestShellCloseKillsForeground(t *testing.T) {
	s, err := startShell("", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("sleep 300\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	s.Close()
	select {
	case <-s.done:
	case <-time.After(6 * time.Second):
		t.Fatal("shell 没有结束")
	}
}

func TestNodeAccessSwitch(t *testing.T) {
	dir := t.TempDir()
	if NodeAccessDisabled(dir) {
		t.Fatal("默认打开")
	}
	if err := SetNodeAccess(dir, false); err != nil {
		t.Fatal(err)
	}
	if !NodeAccessDisabled(dir) {
		t.Fatal("应该已关闭")
	}
	if err := SetNodeAccess(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeAccess(dir, true); err != nil {
		t.Fatal("重复打开不算错：", err)
	}
	if NodeAccessDisabled(dir) {
		t.Fatal("应该已打开")
	}
}
