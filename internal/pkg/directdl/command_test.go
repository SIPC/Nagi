package directdl

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var update = flag.Bool("update", false, "重新生成 testdata 里的命令样本")

// 恶意的文件名：shell 和 PowerShell 的特殊字符、弯引号、Windows 不允许的字符、设备名、结尾的点和空格。
var hostileNames = []string{
	"it's $(touch pwned) `touch pwned2`; & | ‘q’ $env:PATH.txt",
	`a<b>:c"d|e?f*\g.bin`,
	"CON.txt",
	"lpt1",
	"trailing. . ",
	"line\nbreak\r\x00.bin",
	"-rf",
}

func goldenKey() ContentKey {
	return FileKey(bytes.Repeat([]byte{7}, 32), Claims{Node: "n", ID: "golden"}, `"1-2"`)
}

// 命令的样本（testdata/*.golden）：文件名怎么引用、清理，脚本的结构有变化时这里失败，确认后用 -update 重新生成。
func TestCommandGolden(t *testing.T) {
	var posix, win strings.Builder
	for i, name := range hostileNames {
		for _, c := range []Command{
			{URL: "http://203.0.113.5:8080/.nagi/dl/abc.def", Name: name, Key: goldenKey(), SHA256: strings.Repeat("a", 64), Resume: true, Size: 1000},
			{URL: "http://[2001:db8::5]/.nagi/dl/abc.def", Name: name, Key: goldenKey(), SHA256: strings.Repeat("b", 64), Resume: true, Size: 64 << 20},
			{URL: "http://203.0.113.5/.nagi/dl/abc.def", Name: name, Key: goldenKey(), Resume: false},
		} {
			fmt.Fprintf(&posix, "#### %d %q size=%d resume=%v\n%s\n", i, name, c.Size, c.Resume, c.String())
			fmt.Fprintf(&win, "#### %d %q size=%d resume=%v\n%s\n", i, name, c.Size, c.Resume, c.Windows())
		}
	}
	for file, got := range map[string]string{"command_posix.golden": posix.String(), "command_windows.golden": win.String()} {
		path := filepath.Join("testdata", file)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v（用 -update 生成）", err)
		}
		if string(want) != got {
			t.Errorf("%s 和样本不同，确认后用 -update 重新生成", file)
		}
	}
}

func TestWinSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":         "a.txt",
		`a<b>:c"d|e?f*`: "a_b__c_d_e_f_",
		`dir\file`:      "dir_file",
		"CON":           "_CON",
		"con.txt":       "_con.txt",
		"Nul .tar.gz":   "_Nul .tar.gz",
		"COM1.log":      "_COM1.log",
		"lpt9":          "_lpt9",
		"COM¹":          "_COM¹",
		"COM10":         "COM10",
		"CONSOLE":       "CONSOLE",
		"name. . ":      "name",
		"...":           "download",
		"tab\there":     "tab_here",
	} {
		if got := WinSafeName(in); got != want {
			t.Errorf("WinSafeName(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestPSQuote(t *testing.T) {
	for in, want := range map[string]string{
		"a":      "'a'",
		"it's":   "'it''s'",
		"‘x’":    "'‘‘x’’'",
		"$a`b\"": "'$a`b\"'",
	} {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestSegmented(t *testing.T) {
	c := Command{Resume: true, Size: MultiThreshold}
	if !c.Segmented() {
		t.Error("不小于阈值的文件应分段")
	}
	c.Size--
	if c.Segmented() || strings.Contains(c.String(), "N=") || strings.Contains(c.Windows(), "$N") {
		t.Error("小文件用一个连接")
	}
	c = Command{Resume: false, Size: 0}
	if c.Segmented() || strings.Contains(c.Windows(), "'-C'") {
		t.Error("目录打包用一个连接、不续传")
	}
}

// rangeServer 像节点那样按 Range 发密文（每段从它的起点取密钥流），记下发出的字节数。interrupt 为 true 时，
// 第一个从 cut 开始的请求只发一半就断开（模拟其中一段下载中断）。
type rangeServer struct {
	plain     []byte
	key       ContentKey
	cut       int64
	interrupt bool

	mu       sync.Mutex
	served   int64
	requests []string
}

func (s *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	size := int64(len(s.plain))
	start, end := int64(0), size-1
	rg := r.Header.Get("Range")
	if rg != "" {
		if n, _ := fmt.Sscanf(rg, "bytes=%d-%d", &start, &end); n == 0 {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= size {
			end = size - 1
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, rg)
	cutNow := s.interrupt && start == s.cut
	if cutNow {
		s.interrupt = false
	}
	s.mu.Unlock()
	out := make([]byte, end-start+1)
	s.key.Stream(start).XORKeyStream(out, s.plain[start:end+1])
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	if rg != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	}
	if cutNow {
		out = out[:len(out)/2]
	}
	n, _ := w.Write(out)
	s.mu.Lock()
	s.served += int64(n)
	s.mu.Unlock()
	if cutNow {
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

// e2eFixture 准备分段下载的内容：IV 靠近 128 位的上限，计数器在下载中跨过回绕。
func e2eFixture(t *testing.T) (*rangeServer, Command, []byte) {
	plain := make([]byte, MultiThreshold+3<<20+12345)
	_, _ = rand.Read(plain)
	sum := sha256.Sum256(plain)
	k := FileKey(bytes.Repeat([]byte{9}, 32), Claims{Node: "n", Path: "x"}, `"1-2"`)
	copy(k.IV[:], unhex(t, "fffffffffffffffffffffffffffff000"))
	seg := (int64(len(plain)) + Threads - 1) / Threads
	srv := &rangeServer{plain: plain, key: k, cut: seg, interrupt: true}
	c := Command{Name: "big 'v1'.bin", Key: k, SHA256: hex.EncodeToString(sum[:]), Resume: true, Size: int64(len(plain))}
	return srv, c, plain
}

func checkE2E(t *testing.T, srv *rangeServer, dir, name string, plain []byte, out []byte, minRequests int) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("解密的内容不对：%v\n%s", err, out)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part") {
			t.Errorf("分段的密文应该删掉：%s", e.Name())
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// 各段不重叠，重试只补没收到的部分：发出的字节不超过链接的上限（文件大小加 4 MiB）。
	if limit := int64(len(plain)) + 4<<20; srv.served > limit {
		t.Errorf("发出 %d 字节，超过上限 %d", srv.served, limit)
	}
	if len(srv.requests) < minRequests {
		t.Errorf("应该分 %d 段（中断的一段再续传）：%q", Threads, srv.requests)
	}
}

// 端到端：用真的 sh、curl、openssl 跑分段下载的脚本，其中一段中途断开、自动接着下载，然后解密、核对 SHA-256。
// 再模拟上一次运行留下的一段：同一条命令重新运行时从断开的地方接着下载。没有这些工具时跳过（Windows 上在容器里跑）。
func TestSegmentedPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	for _, tool := range []string{"sh", "curl", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s", tool)
		}
	}
	srv, c, plain := e2eFixture(t)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	c.URL = hs.URL + HTTPPathPrefix + "tok"
	if !strings.Contains(c.String(), "curl -4 ") {
		t.Errorf("应该写明 -4：%s", c.String())
	}
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", c.String())
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
		t.Fatalf("命令失败：%v\n%s", err, out)
	}
	checkE2E(t, srv, dir, SafeName(c.Name), plain, out, Threads+1)

	// 上一次运行留下第 3 段的前一部分：只请求剩下的。
	srv2, _, _ := e2eFixture(t)
	srv2.plain, srv2.key, srv2.interrupt = plain, c.Key, false
	hs2 := httptest.NewServer(srv2)
	defer hs2.Close()
	c.URL = hs2.URL + HTTPPathPrefix + "tok"
	dir = t.TempDir()
	seg := (c.Size + Threads - 1) / Threads
	enc := make([]byte, 1000)
	c.Key.Stream(2*seg).XORKeyStream(enc, plain[2*seg:2*seg+1000])
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.%d.part2", c.partBase(SafeName(c.Name)), Threads)), enc, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", "-c", c.String())
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
		t.Fatalf("续传失败：%v\n%s", err, out)
	}
	srv2.mu.Lock()
	want := fmt.Sprintf("bytes=%d-%d", 2*seg+1000, 3*seg-1)
	found := false
	for _, r := range srv2.requests {
		found = found || r == want
	}
	srv2.mu.Unlock()
	if !found {
		t.Errorf("第 3 段应该从 %s 接着下载：%q", want, srv2.requests)
	}
	checkE2E(t, srv2, dir, SafeName(c.Name), plain, out, Threads)
}

// 端到端：用 PowerShell（pwsh）跑 Windows 版的脚本，curl.exe 指向 curl。分段下载（中途断开一段）和一个连接的续传，
// 解密用 C# 的 CTR 实现，结果要和 Go 的密钥流一致，并核对 SHA-256。没有 pwsh 时跳过（在 PowerShell 的容器里跑）。
func TestWindowsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("不在宿主机上运行下载脚本，在容器里跑")
	}
	for _, tool := range []string{"pwsh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s", tool)
		}
	}
	curl, _ := exec.LookPath("curl")
	bin := t.TempDir()
	if err := os.Symlink(curl, filepath.Join(bin, "curl.exe")); err != nil {
		t.Fatal(err)
	}
	// asFile 为 false 时整段作为 -Command 的参数运行，接近粘贴进窗口。
	run := func(dir, script string, asFile bool) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "dl.ps1")
		if err := os.WriteFile(file, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("pwsh", "-NoProfile", "-NonInteractive", "-File", file)
		if !asFile {
			cmd = exec.Command("pwsh", "-NoProfile", "-NonInteractive", "-Command", script)
		}
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	srv, c, plain := e2eFixture(t)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	c.URL = hs.URL + HTTPPathPrefix + "tok"
	dir := t.TempDir()
	out := run(dir, c.Windows(), true)
	if !strings.Contains(out, "SHA-256 OK") {
		t.Fatalf("分段下载失败：\n%s", out)
	}
	checkE2E(t, srv, dir, WinSafeName(SafeName(c.Name)), plain, []byte(out), Threads+1)

	// 一个连接（小文件）：先放一部分密文，续传后解密。文件名带引号和 PowerShell 的特殊字符。
	small := plain[:5<<20+3]
	sum := sha256.Sum256(small)
	srv3 := &rangeServer{plain: small, key: c.Key}
	hs3 := httptest.NewServer(srv3)
	defer hs3.Close()
	c3 := Command{URL: hs3.URL + HTTPPathPrefix + "tok", Name: "it's ‘a’ $(x) `y`.bin", Key: c.Key, SHA256: hex.EncodeToString(sum[:]), Resume: true, Size: int64(len(small))}
	dir = t.TempDir()
	enc := make([]byte, 1<<20+7)
	c.Key.Stream(0).XORKeyStream(enc, small[:len(enc)])
	name := WinSafeName(SafeName(c3.Name))
	if err := os.WriteFile(filepath.Join(dir, c3.partBase(name)+".part"), enc, 0o644); err != nil {
		t.Fatal(err)
	}
	out = run(dir, c3.Windows(), false)
	if !strings.Contains(out, "SHA-256 OK") {
		t.Fatalf("一个连接的下载失败：\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, small) {
		t.Fatalf("解密的内容不对：%v\n%s", err, out)
	}
	if srv3.requests[0] != fmt.Sprintf("bytes=%d-", len(enc)) {
		t.Errorf("应该续传：%q", srv3.requests)
	}
	// 内容不对时提示失败。
	c3.SHA256 = strings.Repeat("0", 64)
	if out := run(t.TempDir(), c3.Windows(), true); !strings.Contains(out, "FAILED") {
		t.Errorf("SHA-256 对不上时应该提示：\n%s", out)
	}
}
