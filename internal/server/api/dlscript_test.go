package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/nodes"
)

// 短命令取脚本（dlscript.go）：令牌只换这一次下载的两份脚本；过期、撤销后给提示重新生成的脚本；不认识的令牌、
// 别的扩展名、明文 HTTP 一律 404；令牌不能当经面板下载的签名链接用。
func TestDownloadScript(t *testing.T) {
	a := &API{Panel: &PanelInfo{Fingerprint: "sha256//abc="}}
	mux := http.NewServeMux()
	mux.Handle("GET /dl/{file}", a.DownloadScriptHandler())
	mux.Handle("GET /api/download/{token}", a.DownloadLinkHandler())
	get := func(path string, secure bool) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	cmd := &nodes.DirectHTTPCommand{Command: "( echo posix )", CommandWindows: "& { 'windows' }", Expires: time.Now().Add(time.Minute)}
	tok := saveScript(cmd)
	other := saveScript(&nodes.DirectHTTPCommand{Command: "( echo other )", CommandWindows: "& { 'other' }", Expires: time.Now().Add(time.Minute)})

	if code, body := get("/dl/"+tok+".sh", true); code != 200 || body != "( echo posix )\n" {
		t.Errorf(".sh：%d %q", code, body)
	}
	if code, body := get("/dl/"+tok+".ps1", true); code != 200 || body != "& { 'windows' }\n" {
		t.Errorf(".ps1：%d %q", code, body)
	}
	if code, body := get("/dl/"+other+".sh", true); code != 200 || !strings.Contains(body, "other") {
		t.Errorf("另一个令牌给另一份脚本：%d %q", code, body)
	}
	for _, path := range []string{"/dl/" + tok + ".txt", "/dl/" + tok, "/dl/x" + tok + ".sh", "/dl/.sh", "/api/download/" + tok} {
		if code, body := get(path, true); code != 404 || strings.Contains(body, "posix") {
			t.Errorf("%s 应该 404：%d %q", path, code, body)
		}
	}
	if code, _ := get("/dl/"+tok+".sh", false); code != 404 {
		t.Errorf("明文 HTTP 应该 404：%d", code)
	}

	// 撤销权限（换直连下载的密钥）以后，没过期的令牌也只给提示。
	expireScripts()
	if code, body := get("/dl/"+tok+".sh", true); code != 200 || !strings.Contains(body, "expired") || strings.Contains(body, "posix") || !strings.Contains(body, "exit 1") {
		t.Errorf("撤销后：%d %q", code, body)
	}
	if code, body := get("/dl/"+tok+".ps1", true); code != 200 || !strings.Contains(body, "expired") || strings.Contains(body, "windows") {
		t.Errorf("撤销后：%d %q", code, body)
	}
	// 过期很久的令牌在下次签发时清掉。
	dlScripts.Lock()
	dlScripts.m[tok].expires = time.Now().Add(-scriptTTLGrace - time.Second)
	dlScripts.Unlock()
	saveScript(cmd)
	if code, _ := get("/dl/"+tok+".sh", true); code != 404 {
		t.Errorf("过期很久的令牌应该清掉：%d", code)
	}
}

// 短命令：按 IP 访问面板时写明证书的公钥指纹（自签证书），按域名访问时照常核对证书；都只走 HTTPS。
func TestShortCommands(t *testing.T) {
	a := &API{Panel: &PanelInfo{Fingerprint: "sha256//abc="}}
	for host, pin := range map[string]bool{"203.0.113.5:8443": true, "[2001:db8::1]:8443": true, "panel.example.com": false} {
		req := httptest.NewRequest(http.MethodGet, "/api/volume-file/direct", nil)
		req.Host = host
		posix, win := a.shortCommands(req, "TOK")
		if posix != "curl -fsSL "+pinFlag(pin)+"'https://"+host+"/dl/TOK.sh' | sh" {
			t.Errorf("%s：%s", host, posix)
		}
		if win != "curl.exe -fsSL "+pinFlag(pin)+"'https://"+host+"/dl/TOK.ps1' | Out-String | iex" {
			t.Errorf("%s：%s", host, win)
		}
	}
}

func pinFlag(pin bool) string {
	if pin {
		return "-k --pinnedpubkey 'sha256//abc=' "
	}
	return ""
}

func TestDownloadScriptCacheBoundsAndExpiry(t *testing.T) {
	reset := func() {
		dlScripts.Lock()
		dlScripts.m = map[string]*dlScript{}
		dlScripts.Unlock()
	}
	reset()
	t.Cleanup(reset)
	a := &API{}
	issue := func(command, windows string, expires time.Time) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "https://panel.test/command", nil)
		w := httptest.NewRecorder()
		a.writeCommand(w, r, &nodes.DirectHTTPCommand{Command: command, CommandWindows: windows, Expires: expires})
		return w
	}
	for _, large := range []bool{false, true} {
		reset()
		command, windows := "sh", "ps1"
		if large {
			command, windows = strings.Repeat("x", 2<<20), strings.Repeat("y", 2<<20)
		}
		n := 0
		for ; n <= 1024; n++ {
			w := issue(command, windows, time.Now().Add(time.Minute))
			if w.Code == http.StatusTooManyRequests {
				break
			}
			if w.Code != http.StatusOK {
				t.Fatalf("issue: %d %s", w.Code, w.Body)
			}
		}
		if (!large && n != 1024) || (large && n != 4) {
			t.Fatalf("cache admission bound: large=%v accepted=%d", large, n)
		}
		expireScripts()
		if w := issue("new", "new-windows", time.Now().Add(time.Minute)); w.Code != http.StatusOK {
			t.Fatalf("expired entries did not free capacity: %d", w.Code)
		}
	}
	reset()
	tok := saveScript(&nodes.DirectHTTPCommand{Command: "secret-sh", CommandWindows: "secret-ps1", Expires: time.Now().Add(50 * time.Millisecond)})
	deadline := time.Now().Add(3 * time.Second)
	for {
		dlScripts.Lock()
		s := dlScripts.m[tok]
		cleared := s != nil && s.posix == "" && s.windows == ""
		dlScripts.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle cache retained expired script bodies")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /dl/{file}", a.DownloadScriptHandler())
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodGet, "https://panel.test/dl/"+tok+".sh", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "expired") || strings.Contains(w.Body.String(), "secret") {
				t.Errorf("expired script response: %d %s", w.Code, w.Body)
			}
		})
		wg.Go(expireScripts)
	}
	wg.Wait()
}
