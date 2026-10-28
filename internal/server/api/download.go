package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/release"
)

// v0.1.0 的节点一键升级时从面板的 /download/nagi-linux-<架构> 下载（钉住面板公钥，不认 UpgradeRequest.version），
// 新版本的节点改从 GitHub Release 下载。为了让旧节点还能在新面板上一键升级，面板保留这个地址：
// 从 GitHub Release 下载面板自己这个版本的二进制，按 SHA256SUMS 核对后缓存在 <面板数据目录>/release/<版本>/ 下再提供。
// 面板不是正式版本（dev）时没有对应的发布文件，返回 404。

var releaseCacheMu sync.Mutex

// DownloadHandler 提供 GET /download/nagi-linux-<架构>，只给旧版本的节点升级用。
func (a *API) DownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arch, ok := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, "/download/"), "nagi-linux-")
		if !ok || (arch != "amd64" && arch != "arm64") || !release.IsVersion(a.Panel.Version) {
			http.NotFound(w, r)
			return
		}
		path, err := cachedRelease(r.Context(), a.Panel.DataDir, a.Panel.Version, "nagi-linux-"+arch)
		if err != nil {
			a.Log.Warn("为旧版本节点准备 nagi 失败", "arch", arch, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	})
}

// cachedRelease 返回某个版本的发布文件在本地缓存里的路径，没有时先从 GitHub Release 下载并核对 SHA-256。
func cachedRelease(ctx context.Context, dataDir, version, name string) (string, error) {
	dir := filepath.Join(dataDir, "release", version)
	path := filepath.Join(dir, name)
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	base := release.DownloadBase(version)
	sums, err := httpGet(ctx, base+"/SHA256SUMS", nil)
	if err != nil {
		return "", err
	}
	want := release.Checksum(string(sums), name)
	if want == "" {
		return "", fmt.Errorf("%s/SHA256SUMS 里没有 %s", base, name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = httpGet(ctx, base+"/"+name, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("%s 校验不通过", name)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// httpGet 下载 url：w 不为空时写进 w，否则读进内存返回（最多 64 KB）。
func httpGet(ctx context.Context, url string, w io.Writer) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败：%s", url, resp.Status)
	}
	if w != nil {
		_, err = io.Copy(w, resp.Body)
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}
