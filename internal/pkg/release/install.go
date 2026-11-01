package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// downloadURLFile 记着安装时用 --download-url 指定的镜像地址（节点在 <节点目录>/identity/ 下，面板在 <面板数据目录>/ 下），
// 一键升级也从这里下载；没有时从 GitHub Release 下载。
const downloadURLFile = "download-url"

// BaseFor 返回 dir 对应的安装下载某个版本时用的地址：安装时指定了镜像就用镜像。
func BaseFor(dir, version string) string {
	if b, err := os.ReadFile(filepath.Join(dir, downloadURLFile)); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimRight(strings.TrimSpace(string(b)), "/")
	}
	return DownloadBase(version)
}

// unusable 表示下载到的文件不能用：在这台机器上运行不了，或者不是要求的版本。
type unusable struct{ error }

func (u unusable) Unwrap() error { return u.error }

// IsUnusable 判断 Install 失败的原因是不是下载到的文件不能用（而不是下载失败）。
func IsUnusable(err error) bool {
	var u unusable
	return errors.As(err, &u)
}

// Install 从 base 下载 want 版本的 nagi，按 SHA256SUMS 核对，试运行确认能用、版本对得上，替换当前的可执行文件，
// 返回它的路径。之后用 Reexec 原地重启。
func Install(ctx context.Context, base, want string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	tmp := exe + ".new"
	if err := Download(ctx, base, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(vctx, tmp, "version").Output()
	cancel()
	if err != nil {
		os.Remove(tmp)
		return "", unusable{i18n.Errorf("新版本在这台机器上运行不了：%w", err)}
	}
	version := strings.TrimSpace(string(out))
	if version != want {
		// 镜像地址不分版本，里面放的可能不是要求的版本。
		os.Remove(tmp)
		return "", unusable{i18n.Errorf("下载到的是 %s，不是要求的 %s（检查镜像地址 %s）", version, want, base)}
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return exe, nil
}

// Download 从 base 下载 nagi-linux-<架构> 到 dst，并按同一地址下的 SHA256SUMS 核对。
func Download(ctx context.Context, base, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	name := "nagi-linux-" + runtime.GOARCH

	sums, err := fetch(ctx, base+"/SHA256SUMS", 64<<10, nil)
	if err != nil {
		return err
	}
	want := Checksum(string(sums), name)
	if want == "" {
		return i18n.Errorf("SHA256SUMS 里没有 %s", name)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = fetch(ctx, base+"/"+name, 0, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return i18n.Errorf("下载的文件校验不通过（%s，应为 %s）", got[:12], want[:min(12, len(want))])
	}
	return nil
}

// fetch 下载 url：w 不为空时写进 w，否则读进内存返回（最多 limit 字节）。
func fetch(ctx context.Context, url string, limit int64, w io.Writer) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, i18n.Errorf("下载 %s 失败：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, i18n.Errorf("下载 %s 失败：%s", url, resp.Status)
	}
	if w != nil {
		if _, err := io.Copy(w, resp.Body); err != nil {
			return nil, i18n.Errorf("下载 %s 失败：%w", url, err)
		}
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, i18n.Errorf("下载 %s 失败：%w", url, err)
	}
	return b, nil
}

// latestURL 是最新发布版本的页面，GitHub 把它重定向到 .../releases/tag/<版本>（不含预发布版）。
// 用重定向而不是 GitHub API：不占 API 的限额（每个 IP 每小时 60 次）。
var latestURL = "https://github.com/" + Repo + "/releases/latest"

// Latest 返回最新发布的版本号。
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	v := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode/100 != 3 || !IsVersion(v) {
		return "", i18n.Errorf("GitHub 没有返回最新版本（%s）", resp.Status)
	}
	return v, nil
}

// PageURL 返回某个版本的发布页面（有这个版本的更新说明）。
func PageURL(version string) string {
	return "https://github.com/" + Repo + "/releases/tag/" + version
}

var versionPartsRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)([-+].*)?$`)

// Older 判断版本 a 是否早于 b。不是正式版本号（例如本地编译的 dev）时不比较，返回 false。
// 和 web/src/pages/nodes.tsx 的 isOlder 一致。
func Older(a, b string) bool {
	x, y := versionPartsRe.FindStringSubmatch(a), versionPartsRe.FindStringSubmatch(b)
	if x == nil || y == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		p, _ := strconv.Atoi(x[i])
		q, _ := strconv.Atoi(y[i])
		if p != q {
			return p < q
		}
	}
	// 数字相同时，预发布版（v0.2.0-rc.1）早于正式版（v0.2.0）。
	return a != b && strings.HasPrefix(x[4], "-") && !strings.HasPrefix(y[4], "-")
}
