package api

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 加密 HTTP 下载的短命令（见 docs/agent.md#短命令）：完整的下载脚本有几十到上百行，签发命令时面板把这次下载的
// 两份脚本存在内存里，给一个随机的令牌，短命令从面板取脚本再执行：
//
//	curl -fsSL 'https://<面板>/dl/<令牌>.sh' | sh
//	curl.exe -fsSL 'https://<面板>/dl/<令牌>.ps1' | Out-String | iex
//
// 令牌只能取这一份脚本，不是面板的凭据：不能用来读别的文件、调用接口，有效期和命令里的链接一样（directdl.TTL）。
// 脚本和完整命令一样，里面有这一个文件的内容密钥和链接，没有长期有效的凭据。只走 HTTPS：面板用自签证书时
// （按 IP 访问）短命令写明证书的公钥指纹（--pinnedpubkey），不信任证书也不会被冒充。过期以后的一段时间里
// 同一个令牌返回一个只提示「已过期，请到面板重新生成」的脚本；不认识的令牌 404。面板重启、撤销权限时作废。

// scriptTTLGrace：过期的令牌再记这么久，用来提示已过期。
const (
	scriptTTLGrace = time.Hour
	scriptEntries  = 1024
	scriptBytes    = 16 << 20
)

const scriptPathPrefix = "/dl/"

type dlScript struct {
	posix, windows string
	expires        time.Time
}

var dlScripts = struct {
	sync.Mutex
	m     map[string]*dlScript
	timer *time.Timer
}{m: map[string]*dlScript{}}

// cleanScriptsLocked drops expired secrets even when nobody requests a script
// or issues another command. Expired notices remain small and bounded.
func cleanScriptsLocked(now time.Time) int {
	size := 0
	for k, s := range dlScripts.m {
		if !now.Before(s.expires.Add(scriptTTLGrace)) {
			delete(dlScripts.m, k)
			continue
		}
		if !now.Before(s.expires) {
			s.posix, s.windows = "", ""
		}
		size += len(s.posix) + len(s.windows)
	}
	return size
}

func cleanScripts() {
	dlScripts.Lock()
	defer dlScripts.Unlock()
	cleanScriptsLocked(time.Now())
	if len(dlScripts.m) > 0 {
		dlScripts.timer.Reset(time.Second)
	} else {
		dlScripts.timer = nil
	}
}

// saveScript 存下这次下载的脚本，返回令牌；缓存已满或单份脚本过大时返回空字符串。
func saveScript(cmd *nodes.DirectHTTPCommand) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	dlScripts.Lock()
	defer dlScripts.Unlock()
	size := cleanScriptsLocked(now)
	if len(dlScripts.m) >= scriptEntries {
		for k, s := range dlScripts.m {
			if !now.Before(s.expires) {
				delete(dlScripts.m, k)
				break
			}
		}
	}
	if len(dlScripts.m) >= scriptEntries || len(cmd.Command)+len(cmd.CommandWindows) > scriptBytes-size {
		return ""
	}
	dlScripts.m[tok] = &dlScript{posix: cmd.Command, windows: cmd.CommandWindows, expires: cmd.Expires}
	if dlScripts.timer == nil {
		dlScripts.timer = time.AfterFunc(time.Second, cleanScripts)
	}
	return tok
}

// expireScripts 让所有令牌立即过期（撤销权限、换直连下载的密钥时，和链接一起失效），之后取脚本时提示重新生成。
func expireScripts() {
	now := time.Now()
	dlScripts.Lock()
	for _, s := range dlScripts.m {
		if s.expires.After(now) {
			s.expires = now
		}
		s.posix, s.windows = "", ""
	}
	dlScripts.Unlock()
}

// 过期的令牌给的脚本。
const (
	expiredPOSIX   = "echo 'FAILED: this download command has expired. Generate a new command in the panel.' >&2; exit 1\n"
	expiredWindows = "Write-Host 'FAILED: this download command has expired. Generate a new command in the panel.'\n"
)

// DownloadScriptHandler：GET /dl/{file}，file 是「<令牌>.sh」或「<令牌>.ps1」。只凭令牌，不看 cookie。
func (a *API) DownloadScriptHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		tok, ext, _ := strings.Cut(file, ".")
		dlScripts.Lock()
		cleanScriptsLocked(time.Now())
		s := dlScripts.m[tok]
		var snapshot dlScript
		if s != nil {
			snapshot = *s
		}
		dlScripts.Unlock()
		if s == nil || (ext != "sh" && ext != "ps1") || r.TLS == nil {
			http.NotFound(w, r)
			return
		}
		s = &snapshot
		body := s.posix
		if ext == "ps1" {
			body = s.windows
		}
		if !time.Now().Before(s.expires) {
			body = expiredPOSIX
			if ext == "ps1" {
				body = expiredWindows
			}
		}
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "private, no-store")
		h.Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write([]byte(body + "\n"))
	})
}

// shortCommands 生成两条短命令。面板按 IP 访问时用的是自签证书，写明公钥指纹；按域名访问时证书由公共 CA 签发
// （面板自己申请、本机 Caddy 或用户自己的反向代理），照常核对。
func (a *API) shortCommands(r *http.Request, tok string) (posix, windows string) {
	host := auth.RequestHost(r)
	base := "https://" + host + scriptPathPrefix + tok
	pin := ""
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if net.ParseIP(strings.Trim(name, "[]")) != nil && a.Panel != nil && a.Panel.Fingerprint != "" {
		pin = "-k --pinnedpubkey '" + a.Panel.Fingerprint + "' "
	}
	return "curl -fsSL " + pin + "'" + base + ".sh' | sh",
		"curl.exe -fsSL " + pin + "'" + base + ".ps1' | Out-String | iex"
}
