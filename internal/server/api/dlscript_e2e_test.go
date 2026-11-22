package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/nodes"
)

// 端到端：在 Windows 上用真的 curl.exe 运行短命令（PowerShell 5.1 和 7 各跑一次），面板是自签证书的 HTTPS，
// 短命令按公钥指纹核对；指纹不对时 curl.exe 拒绝连接，脚本不执行。没有 Windows 时跳过（Linux 的短命令用 sh，
// 见 TestShortCommandPOSIX）。
func TestShortCommandWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("要在 Windows 上运行")
	}
	shortCommandE2E(t, []string{"powershell", "pwsh"}, func(host, cmd string) *exec.Cmd {
		return exec.Command(host, "-NoProfile", "-NonInteractive", "-Command", cmd)
	}, true)
}

// 端到端：Linux、macOS 上用 sh 运行短命令。
func TestShortCommandPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("没有 curl")
	}
	shortCommandE2E(t, []string{"sh"}, func(host, cmd string) *exec.Cmd { return exec.Command(host, "-c", cmd) }, false)
}

func shortCommandE2E(t *testing.T, hosts []string, run func(host, cmd string) *exec.Cmd, windows bool) {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	a := &API{Panel: &PanelInfo{Fingerprint: "sha256//" + base64.StdEncoding.EncodeToString(sum[:])}}
	mux.Handle("GET /dl/{file}", a.DownloadScriptHandler())
	host := strings.TrimPrefix(srv.URL, "https://")
	marker := filepath.Join(t.TempDir(), "ran")
	tok := saveScript(&nodes.DirectHTTPCommand{
		Command:        "( echo 'script ran' > '" + marker + "' && echo 'SHA-256 OK' )",
		CommandWindows: "& {\n$ErrorActionPreference = 'Stop'\nSet-Content -LiteralPath '" + marker + "' -Value 'script ran'\n'SHA-256 OK'\n}",
		Expires:        time.Now().Add(time.Minute),
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	posix, win := a.shortCommands(req, tok)
	cmd := posix
	if windows {
		cmd = win
	}
	for _, h := range hosts {
		if _, err := exec.LookPath(h); err != nil {
			continue
		}
		_ = os.Remove(marker)
		out, err := run(h, cmd).CombinedOutput()
		if b, _ := os.ReadFile(marker); err != nil || !strings.Contains(string(out), "SHA-256 OK") || !strings.Contains(string(b), "script ran") {
			t.Errorf("%s：短命令没有运行脚本：%v\n%s\n%s", h, err, cmd, out)
		}
		// 指纹不对（被冒充的面板）：不执行。
		_ = os.Remove(marker)
		bad := strings.Replace(cmd, a.Panel.Fingerprint, "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 1)
		out, _ = run(h, bad).CombinedOutput()
		if _, err := os.Stat(marker); err == nil || strings.Contains(string(out), "SHA-256 OK") {
			t.Errorf("%s：指纹不对时不应该执行：\n%s", h, out)
		}
		// 过期以后提示重新生成。
		if h == hosts[0] {
			expireScripts()
			out, _ = run(h, cmd).CombinedOutput()
			if !strings.Contains(string(out), "expired") || !strings.Contains(string(out), "Generate a new command") {
				t.Errorf("%s：过期时应该提示：\n%s", h, out)
			}
			dlScripts.Lock()
			dlScripts.m[tok].expires = time.Now().Add(time.Minute)
			dlScripts.Unlock()
		}
	}
}
