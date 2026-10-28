package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadHandler(t *testing.T) {
	dir := t.TempDir()
	get := func(version, path string) int {
		a := &API{Panel: &PanelInfo{DataDir: dir, Version: version}}
		rec := httptest.NewRecorder()
		a.DownloadHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	// 本地编译的面板没有对应的发布文件；不认识的文件名一律 404。
	for _, c := range []struct{ version, path string }{
		{"dev", "/download/nagi-linux-amd64"},
		{"v0.2.0", "/download/nagi-linux-386"},
		{"v0.2.0", "/download/index.html"},
	} {
		if code := get(c.version, c.path); code != http.StatusNotFound {
			t.Errorf("%s %s：%d，应为 404", c.version, c.path, code)
		}
	}
	// 已经缓存的版本直接提供，不再访问 GitHub。
	cached := filepath.Join(dir, "release", "v0.2.0", "nagi-linux-arm64")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("bin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := get("v0.2.0", "/download/nagi-linux-arm64"); code != http.StatusOK {
		t.Errorf("已缓存的文件：%d，应为 200", code)
	}
}
