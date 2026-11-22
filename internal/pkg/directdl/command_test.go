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
	if c.Segmented() || !strings.Contains(c.String(), "N=1\n") || !strings.Contains(c.Windows(), "$N = 1\n") {
		t.Error("小文件用一个连接（1 段）")
	}
	c = Command{Resume: false, Size: 0}
	if c.Segmented() || strings.Contains(c.Windows(), "'-r'") {
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

// psHosts 是机器上能用的 PowerShell：Windows 上是自带的 Windows PowerShell 5.1 和装了的 PowerShell 7；
// 别的系统上是 pwsh（curl.exe 指向 curl）。
func psHosts(t *testing.T) (hosts []string, env []string) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil && runtime.GOOS != "windows" {
		t.Skip("没有 curl")
	}
	for _, h := range []string{"powershell", "pwsh"} {
		if runtime.GOOS != "windows" && h == "powershell" {
			continue
		}
		if _, err := exec.LookPath(h); err == nil {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		t.Skip("没有 PowerShell")
	}
	env = os.Environ()
	if runtime.GOOS != "windows" {
		curl, _ := exec.LookPath("curl")
		bin := t.TempDir()
		if err := os.Symlink(curl, filepath.Join(bin, "curl.exe")); err != nil {
			t.Fatal(err)
		}
		env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return hosts, env
}

// runPS 运行 Windows 版的脚本。asFile 为 false 时整段作为 -Command 的参数运行，接近粘贴进窗口。cwd 是当前目录，
// saveDir 不为空时经 NAGI_DOWNLOAD_DIR 指定保存的位置。
func runPS(t *testing.T, host string, env []string, cwd, saveDir, script string, asFile bool) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "dl.ps1")
	if err := os.WriteFile(file, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(host, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	if !asFile {
		cmd = exec.Command(host, "-NoProfile", "-NonInteractive", "-Command", script)
	}
	cmd.Dir = cwd
	cmd.Env = append(append([]string(nil), env...), "NAGI_DOWNLOAD_DIR="+saveDir)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// 端到端：用 PowerShell 跑 Windows 版的脚本（Windows 上是真的 curl.exe，Windows PowerShell 5.1 和 PowerShell 7 都跑）。
// 分段下载（中途断开一段，自动接着下载）、保存目录的路径带空格和中文、一个连接时的续传，解密用 C# 的 CTR 实现，
// 结果要和 Go 的密钥流一致，并核对 SHA-256；目录不可写、链接被拒绝时说明原因。
func TestWindowsScript(t *testing.T) {
	hosts, env := psHosts(t)
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			srv, c, plain := e2eFixture(t)
			hs := httptest.NewServer(srv)
			defer hs.Close()
			c.URL = hs.URL + HTTPPathPrefix + "tok"
			c.Name = "big 'v1' 中文.bin"
			save := filepath.Join(t.TempDir(), "my files 下载")
			out := runPS(t, host, env, t.TempDir(), save, c.Windows(), true)
			if !strings.Contains(out, "SHA-256 OK") {
				t.Fatalf("分段下载失败：\n%s", out)
			}
			name := WinSafeName(SafeName(c.Name))
			if !strings.Contains(out, "Saved to "+filepath.Join(save, name)) {
				t.Errorf("应该写明保存的位置：\n%s", out)
			}
			checkE2E(t, srv, save, name, plain, []byte(out), Threads+1)

			// 一个连接（小文件）：先放一部分密文，续传后解密。文件名带引号和 PowerShell 的特殊字符。
			small := plain[:5<<20+3]
			sum := sha256.Sum256(small)
			srv3 := &rangeServer{plain: small, key: c.Key}
			hs3 := httptest.NewServer(srv3)
			defer hs3.Close()
			c3 := Command{URL: hs3.URL + HTTPPathPrefix + "tok", Name: "it's ‘a’ $(x) `y`.bin", Key: c.Key, SHA256: hex.EncodeToString(sum[:]), Resume: true, Size: int64(len(small))}
			dir := t.TempDir()
			enc := make([]byte, 1<<20+7)
			c.Key.Stream(0).XORKeyStream(enc, small[:len(enc)])
			name = WinSafeName(SafeName(c3.Name))
			if err := os.WriteFile(filepath.Join(dir, c3.partBase(name)+".1.part0"), enc, 0o644); err != nil {
				t.Fatal(err)
			}
			out = runPS(t, host, env, t.TempDir(), dir, c3.Windows(), false)
			if !strings.Contains(out, "SHA-256 OK") {
				t.Fatalf("一个连接的下载失败：\n%s", out)
			}
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(got, small) {
				t.Fatalf("解密的内容不对：%v\n%s", err, out)
			}
			srv3.mu.Lock()
			if want := fmt.Sprintf("bytes=%d-%d", len(enc), len(small)-1); len(srv3.requests) != 1 || srv3.requests[0] != want {
				t.Errorf("应该续传（%s）：%q", want, srv3.requests)
			}
			srv3.mu.Unlock()
			// 内容不对时提示失败。
			c3.SHA256 = strings.Repeat("0", 64)
			if out := runPS(t, host, env, t.TempDir(), t.TempDir(), c3.Windows(), true); !strings.Contains(out, "FAILED: SHA-256 mismatch") {
				t.Errorf("SHA-256 对不上时应该提示：\n%s", out)
			}
			// 节点拒绝链接（404）：每段只说一次原因，不重试，提示重新生成；之前下载的部分留着。
			srv404 := httptest.NewServer(http.NotFoundHandler())
			defer srv404.Close()
			c.URL = srv404.URL + HTTPPathPrefix + "tok"
			out = runPS(t, host, env, t.TempDir(), t.TempDir(), c.Windows(), true)
			if strings.Count(out, "rejected the link") != 1 || !strings.Contains(out, "download incomplete") {
				t.Errorf("404 时应该说明一次原因：\n%s", out)
			}
		})
	}
}

// Windows 上实测保存的位置：在 C:\Windows\System32（普通用户不可写，就是用户报告的情况）运行时存到下载文件夹，
// 不再 curl: (23)；NAGI_DOWNLOAD_DIR 指向不可写的目录时说明「folder not writable」，不开始下载。
// 以管理员身份运行时 System32 可写，这里只检查默认不写当前目录。
func TestWindowsScriptSaveDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("要在 Windows 上运行")
	}
	hosts, env := psHosts(t)
	plain := []byte("hello from nagi")
	sum := sha256.Sum256(plain)
	k := FileKey(bytes.Repeat([]byte{3}, 32), Claims{Node: "n", Path: "s"}, `"1-2"`)
	srv := &rangeServer{plain: plain, key: k}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	name := "nagi-test-" + NewID() + ".txt"
	c := Command{URL: hs.URL + HTTPPathPrefix + "tok", Name: name, Key: k, SHA256: hex.EncodeToString(sum[:]), Resume: true, Size: int64(len(plain))}
	sys32 := filepath.Join(os.Getenv("SystemRoot"), "System32")
	profile := t.TempDir()
	for _, host := range hosts {
		// 下载文件夹放到临时的用户目录（USERPROFILE），不动真正的下载文件夹。
		env := append(append([]string(nil), env...), "USERPROFILE="+profile)
		cmd := exec.Command(host, "-NoProfile", "-NonInteractive", "-Command",
			"$k = 'HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Explorer\\User Shell Folders'; (Get-ItemProperty -LiteralPath $k).'{374DE290-123F-4565-9164-39C4925E467B}'")
		cmd.Env = env
		reg, _ := cmd.Output()
		want := filepath.Join(profile, "Downloads")
		if s := strings.TrimSpace(string(reg)); s != "" {
			want = s
		}
		file := filepath.Join(t.TempDir(), "dl.ps1")
		if err := os.WriteFile(file, []byte(c.Windows()), 0o644); err != nil {
			t.Fatal(err)
		}
		run := exec.Command(host, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
		run.Dir = sys32
		run.Env = env
		out, _ := run.CombinedOutput()
		if !strings.Contains(string(out), "SHA-256 OK") || !strings.Contains(string(out), "Saved to "+filepath.Join(want, name)) {
			t.Errorf("%s：在 System32 里运行应该存到下载文件夹 %s：\n%s", host, want, out)
		}
		_ = os.Remove(filepath.Join(want, name))
		if _, err := os.Stat(filepath.Join(sys32, name)); err == nil {
			_ = os.Remove(filepath.Join(sys32, name))
			t.Errorf("%s：不该写进当前目录", host)
		}
		out2 := runPS(t, host, env, t.TempDir(), filepath.Join(sys32, "nagi-no-write"), c.Windows(), true)
		if isAdmin() {
			continue
		}
		if !strings.Contains(out2, "folder not writable") || strings.Contains(out2, "Saving to") {
			t.Errorf("%s：不可写的目录应该说明：\n%s", host, out2)
		}
	}
}

// isAdmin：Windows 上当前进程是不是以管理员身份运行（能写 System32）。
func isAdmin() bool {
	f, err := os.CreateTemp(filepath.Join(os.Getenv("SystemRoot"), "System32"), "nagi-admin-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
