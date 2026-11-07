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
	p, err := Prepare(ctx, base, want)
	if err != nil {
		return "", err
	}
	return p.Commit()
}

// Prepared 是下载好、核对过的新版本，还没有替换。
type Prepared struct {
	exe, tmp string
}

// Prepare 做 Install 替换之前的部分：下载、核对、试运行。之后用 Commit 替换，或者 Discard 放弃。
func Prepare(ctx context.Context, base, want string) (*Prepared, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	p := &Prepared{exe: exe, tmp: exe + ".new"}
	if err := Download(ctx, base, p.tmp); err != nil {
		p.Discard()
		return nil, err
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(vctx, p.tmp, "version").Output()
	cancel()
	if err != nil {
		p.Discard()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unusable{i18n.Detail(i18n.New("新版本无法在此机器上运行"), err)}
	}
	version := strings.TrimSpace(string(out))
	if version != want {
		// 镜像地址不分版本，里面放的可能不是要求的版本。
		p.Discard()
		return nil, unusable{i18n.Errorf("下载到的版本是 %s，不是 %s，请检查镜像地址 %s", version, want, base)}
	}
	return p, nil
}

// Commit 用新版本替换当前的可执行文件，返回它的路径。
func (p *Prepared) Commit() (string, error) {
	if err := os.Rename(p.tmp, p.exe); err != nil {
		p.Discard()
		return "", err
	}
	return p.exe, nil
}

// Discard 删掉下载的新版本。
func (p *Prepared) Discard() { os.Remove(p.tmp) }

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
		return i18n.Errorf("SHA256SUMS 中没有 %s，请检查镜像地址", name)
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
		return i18n.Errorf("下载的文件校验不一致（%s，应为 %s），请重试或检查镜像地址", got[:12], want[:min(12, len(want))])
	}
	return nil
}

// fetch 下载 url：w 不为空时写进 w，否则读进内存返回（最多 limit 字节）。
func fetch(ctx context.Context, url string, limit int64, w io.Writer) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	name := path.Base(url)
	unreachable := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return i18n.Detail(i18n.Errorf("无法下载 %s，请检查网络或镜像地址", name), err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, unreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, i18n.Errorf("无法下载 %s（HTTP %d），请检查镜像地址", name, resp.StatusCode)
	}
	if w != nil {
		if _, err := io.Copy(w, resp.Body); err != nil {
			return nil, unreachable(err)
		}
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, unreachable(err)
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
		return "", i18n.Detail(i18n.New("无法连接 GitHub，请检查面板的网络后重试"), err)
	}
	defer resp.Body.Close()
	v := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode/100 != 3 || !IsVersion(v) {
		return "", i18n.Errorf("GitHub 没有返回最新版本（HTTP %d），请稍后重试", resp.StatusCode)
	}
	return v, nil
}

// PageURL 返回某个版本的发布页面（有这个版本的更新说明）。
func PageURL(version string) string {
	return "https://github.com/" + Repo + "/releases/tag/" + version
}

var versionPartsRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)([-+].*)?$`)

// Older 判断版本 a 是否早于 b。不是正式版本号（例如本地编译的 dev）时不比较，返回 false。
// 和 web/src/pages/update.tsx 的 isOlder 一致。
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
