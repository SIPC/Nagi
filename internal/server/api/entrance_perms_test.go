package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/server/auth"
)

// 不走 Connect 的入口按同一套权限检查（见 docs/panel.md#成员和角色）：容器终端要「终端」，上传要「部署」，
// 卷文件的下载要「文件查看」、修改要「文件修改」，直连下载要「文件查看」，节点终端和节点文件要「节点终端和文件」。
func TestEntrancePermissions(t *testing.T) {
	h := newLinkHarness(t)
	h.mux.Handle("/ws/exec", h.a.TerminalHandler())
	h.mux.Handle("/ws/node-shell", h.a.NodeShellHandler())
	h.mux.Handle("/api/node-file", h.a.NodeFileHandler())
	h.mux.Handle("GET /api/node-file/direct", h.a.NodeDirectHandler())
	h.mux.Handle("GET /api/volume-file/direct", h.a.VolumeDirectHandler())
	h.mux.Handle("POST /api/upload", h.a.UploadHandler())
	h.mux.Handle("DELETE /api/volume-file", h.a.VolumeFileHandler())

	// 只有「查看」「日志」的角色。
	h.exec(`INSERT INTO roles (id, name) VALUES (10, '看日志')`)
	h.exec(`INSERT INTO role_permissions (role_id, permission) VALUES (10, 'project.view'), (10, 'project.logs'), (10, 'node.view')`)
	h.exec(`UPDATE users SET role_id = 10 WHERE id = 2`)
	token, _ := h.login(2)
	do := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		req.RemoteAddr = linkIP + ":5000"
		req.Header.Set("User-Agent", chromeUA)
		req.Header.Set("Cookie", auth.CookieName+"="+token)
		req.Header.Set("X-Nagi-Upload", "1")
		rec := httptest.NewRecorder()
		// 权限检查过了以后，处理函数会用到这里没有搭的部分（项目、节点）：走到那一步就算权限够了。
		passed := false
		func() {
			defer func() { passed = recover() != nil }()
			h.mux.ServeHTTP(rec, req)
		}()
		if passed {
			return 0, "passed permission checks"
		}
		return rec.Code, rec.Body.String()
	}
	paths := []struct{ method, path string }{
		{http.MethodGet, "/ws/exec?project=p1&container=web"},
		{http.MethodPost, "/api/upload?project=p1&container=web"},
		{http.MethodGet, "/api/volume-file?project=p1&container=web&volume=data&path=a"},
		{http.MethodDelete, "/api/volume-file?project=p1&container=web&volume=data&path=a"},
		{http.MethodGet, "/api/volume-file/direct?project=p1&container=web&volume=data&path=a"},
		{http.MethodGet, "/ws/node-shell?node=n1"},
		{http.MethodGet, "/api/node-file?node=n1&path=/etc/hostname"},
		{http.MethodGet, "/api/node-file/direct?node=n1&path=/etc/hostname"},
	}
	for _, p := range paths {
		if code, body := do(p.method, p.path); code != http.StatusForbidden {
			t.Errorf("只有查看和日志：%s %s = %d %s，应被拒绝", p.method, p.path, code, body)
		}
	}
	// 预置的操作员：终端、上传、卷文件的权限够了（后面因为没有容器、节点不在线而失败，不是 403）；节点终端和文件仍然不行。
	h.exec(`UPDATE users SET role_id = 2 WHERE id = 2`)
	for _, p := range paths[:5] {
		if code, body := do(p.method, p.path); code == http.StatusForbidden && strings.Contains(body, "没有权限") {
			t.Errorf("操作员：%s %s = %d %s，权限应该够", p.method, p.path, code, body)
		}
	}
	for _, p := range paths[5:] {
		if code, _ := do(p.method, p.path); code != http.StatusForbidden {
			t.Errorf("操作员：%s %s = %d，节点终端和文件应被拒绝", p.method, p.path, code)
		}
	}
}
