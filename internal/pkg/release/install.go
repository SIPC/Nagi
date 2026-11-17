package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// downloadURLFile 记着安装时用 --download-url 指定的镜像地址（节点在 <节点目录>/identity/ 下，面板在 <面板数据目录>/ 下），
// 一键升级也从这里下载；没有时从 GitHub Release 下载。
const downloadURLFile = "download-url"

// githubProxyFile 记着上一次下载 nagi 时用上的 GitHub 加速代理（地址前缀，例如 https://ghfast.top/），
// 和 download-url 在同一个目录。安装脚本在中国大陆网络经代理下载时写入；一键升级时先经它下载，
// 换了地址才下载成功时改成那一个，直接从 GitHub 下载成功时删掉（见 Sources、remember）。
const githubProxyFile = "github-proxy"

// BaseFor 返回 dir 对应的安装下载某个版本时首先用的地址（见 Sources）：安装时指定了镜像就用镜像，
// 记着加速代理就经代理下载 GitHub Release，否则直接从 GitHub 下载。
func BaseFor(dir, version string) string {
	return Sources(dir, version)[0].Base
}

// ProxyFor 返回 dir 对应的安装记着的 GitHub 加速代理，没有时为空。
func ProxyFor(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, githubProxyFile))
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(b))
	if !strings.HasPrefix(p, "https://") {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// unusable 表示下载到的文件不能用：在这台机器上运行不了，或者不是要求的版本。
type unusable struct{ error }

func (u unusable) Unwrap() error { return u.error }

// IsUnusable 判断 Install 失败的原因是不是下载到的文件不能用（而不是下载失败）。
func IsUnusable(err error) bool {
	var u unusable
	return errors.As(err, &u)
}

// Install 下载 want 版本的 nagi（依次尝试 Sources(dir, want) 里的地址），按 SHA256SUMS 核对，试运行确认能用、
// 版本对得上，替换当前的可执行文件，返回它的路径和用上的地址。之后用 Reexec 原地重启。
func Install(ctx context.Context, dir, want string, log *slog.Logger) (string, Source, error) {
	p, err := Prepare(ctx, dir, want, log)
	if err != nil {
		return "", Source{}, err
	}
	exe, err := p.Commit()
	return exe, p.Source, err
}

// Prepared 是下载好、核对过的新版本，还没有替换。
type Prepared struct {
	exe, tmp string
	// Source 是下载用上的地址。
	Source Source
}

// Prepare 做 Install 替换之前的部分：依次尝试 Sources(dir, want) 里的地址下载、核对、试运行，
// 成功后记下用上的地址（见 remember）。之后用 Commit 替换，或者 Discard 放弃。
func Prepare(ctx context.Context, dir, want string, log *slog.Logger) (*Prepared, error) {
	if log == nil {
		log = slog.Default()
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	p := &Prepared{exe: exe, tmp: exe + ".new"}
	src, err := fetchFirst(ctx, Sources(dir, want), p.tmp, func(s Source) error { return p.check(ctx, s.Base, want) }, log)
	if err != nil {
		p.Discard()
		return nil, err
	}
	p.Source = src
	if err := remember(dir, src); err != nil {
		log.Warn("无法记下下载用的地址", "source", src.Name(), "err", err)
	}
	return p, nil
}

// check 试运行下载的新版本，确认能在本机运行、版本是 want。
func (p *Prepared) check(ctx context.Context, base, want string) error {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(vctx, p.tmp, "version").Output()
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return unusable{i18n.Detail(i18n.New("新版本无法在此机器上运行"), err)}
	}
	version := strings.TrimSpace(string(out))
	if version != want {
		// 镜像地址不分版本，里面放的可能不是要求的版本。
		return unusable{i18n.Errorf("下载到的版本是 %s，不是 %s，请检查镜像地址 %s", version, want, base)}
	}
	return nil
}

// Commit 用新版本替换当前的可执行文件，返回它的路径。替换前把当前版本留成 <可执行文件>.old：
// 新版本试运行通过、正式启动却起不来时，节点用它换回去（RestorePrevious，见 agent/upgrade_guard.go）。
func (p *Prepared) Commit() (string, error) {
	if err := keepPrevious(p.exe); err != nil {
		p.Discard()
		return "", i18n.Detail(i18n.New("无法保留当前版本，已停止升级"), err)
	}
	if err := os.Rename(p.tmp, p.exe); err != nil {
		p.Discard()
		return "", err
	}
	return p.exe, nil
}

// keepPrevious 把 exe 留成 exe.old：同一个文件系统上用硬链接（替换后旧文件还在），不行时复制一份。
func keepPrevious(exe string) error {
	old := exe + ".old"
	_ = os.Remove(old)
	if os.Link(exe, old) == nil {
		return nil
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(old+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(old + ".tmp")
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(old + ".tmp")
		return err
	}
	return os.Rename(old+".tmp", old)
}

// RestorePrevious 换回 Commit 留下的上一个版本，返回可执行文件的路径，之后用 Reexec 重启。
func RestorePrevious() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	return exe, restorePrevious(exe)
}

func restorePrevious(exe string) error { return os.Rename(exe+".old", exe) }

// Discard 删掉下载的新版本。
func (p *Prepared) Discard() { os.Remove(p.tmp) }

// Download 从 base 下载 nagi-linux-<架构> 到 dst，并按同一地址下的 SHA256SUMS 核对。
func Download(ctx context.Context, base, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	_, err := download(ctx, base, dst, "")
	return err
}

// download 从 base 下载 nagi-linux-<架构> 到 dst，并按同一地址下的 SHA256SUMS 核对；pinned 不为空时
// SHA256SUMS 里的值还必须等于它（直接从 GitHub 拿到的值，见 fetchFirst）。返回 SHA256SUMS 里的值，下载文件失败时也返回。
func download(ctx context.Context, base, dst, pinned string) (string, error) {
	name := binaryName()

	sums, err := fetch(ctx, base+"/SHA256SUMS", 64<<10, nil)
	if err != nil {
		return "", err
	}
	want := Checksum(string(sums), name)
	if want == "" {
		return "", i18n.Errorf("SHA256SUMS 中没有 %s，请检查镜像地址", name)
	}
	if pinned != "" && want != pinned {
		return want, i18n.Errorf("%s 提供的 SHA256SUMS 和 GitHub 上的不一致，已跳过", hostOf(base))
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return want, err
	}
	h := sha256.New()
	_, err = fetch(ctx, base+"/"+name, 0, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return want, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return want, i18n.Errorf("下载的文件校验不一致（%s，应为 %s），请重试或检查镜像地址", got[:12], want[:min(12, len(want))])
	}
	return want, nil
}

// binaryName 是本机架构的发布文件名。
func binaryName() string { return "nagi-linux-" + runtime.GOARCH }

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

const (
	// connectTimeout 是连接（含 TLS 握手）的时间上限，连不上就换下一个地址。和安装脚本的 curl --connect-timeout 10 一样。
	connectTimeout = 10 * time.Second
	// headerTimeout 是发出请求后等响应头的时间上限。
	headerTimeout = 30 * time.Second
)

// 和安装脚本的 curl --speed-limit 1024 --speed-time 30 一样：连续 lowSpeedTime 内平均每秒不到 lowSpeedLimit 字节就放弃。
var (
	lowSpeedLimit = 1024
	lowSpeedTime  = 30 * time.Second
)

var errTooSlow = errors.New("download too slow")

// statusError 是服务器回的 HTTP 状态不是 200。文字和包着的错误一样（按语言显示时用它的译文）。
type statusError struct {
	code int
	error
}

func (e statusError) Unwrap() error { return e.error }

// transport 是下载用的连接：在 http.DefaultTransport 的基础上（照常用 HTTPS_PROXY 等环境变量）缩短连接和等响应头的时间。
var transport http.RoundTripper = func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = connectTimeout
	t.ResponseHeaderTimeout = headerTimeout
	return t
}()

// fetch 下载 url：w 不为空时写进 w，否则读进内存返回（最多 limit 字节）。速度太慢时放弃（见 watchSpeed）。
func fetch(ctx context.Context, url string, limit int64, w io.Writer) ([]byte, error) {
	name := path.Base(url)
	wctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var got, total atomic.Int64
	total.Store(-1)
	done := make(chan struct{})
	defer close(done)
	deadline, _ := ctx.Deadline()
	// 限速的参数在这里读好再交给 watchSpeed：它可能在 fetch 返回之后才开始运行（测试里会改这两个变量）。
	go watchSpeed(&got, &total, deadline, lowSpeedTime, lowSpeedLimit, done, func() { stop(errTooSlow) })

	req, err := http.NewRequestWithContext(wctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	unreachable := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(context.Cause(wctx), errTooSlow) {
			return i18n.Errorf("下载 %s 的速度过慢，已停止", name)
		}
		return i18n.Detail(i18n.Errorf("无法下载 %s，请检查网络或镜像地址", name), err)
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, unreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError{resp.StatusCode, i18n.Errorf("无法下载 %s（HTTP %d），请检查镜像地址", name, resp.StatusCode)}
	}
	total.Store(resp.ContentLength)
	body := &countingReader{r: resp.Body, n: &got}
	if w != nil {
		if _, err := io.Copy(w, body); err != nil {
			return nil, unreachable(err)
		}
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return nil, unreachable(err)
	}
	return b, nil
}

// watchSpeed 每 lowSpeedTime 看一次下载了多少（got），两种情况调用 abort：
//   - 这段时间平均每秒不到 lowSpeedLimit 字节（卡住了）；
//   - 知道文件大小（total）和时间上限（deadline）时，按开始以来的平均速度在上限之前下载不完：
//     与其耗到超时，不如早点换下一个地址（例如中国大陆直连 GitHub，一直有数据但很慢）。
//
// done 关闭后退出。
func watchSpeed(got, total *atomic.Int64, deadline time.Time, lowSpeedTime time.Duration, lowSpeedLimit int, done <-chan struct{}, abort func()) {
	t := time.NewTicker(lowSpeedTime)
	defer t.Stop()
	start := time.Now()
	least := int64(float64(lowSpeedLimit) * lowSpeedTime.Seconds())
	var last int64
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			n := got.Load()
			if n-last < least {
				abort()
				return
			}
			last = n
			if size := total.Load(); n > 0 && size > n && !deadline.IsZero() {
				left := time.Duration(float64(now.Sub(start)) * float64(size-n) / float64(n))
				if now.Add(left).After(deadline) {
					abort()
					return
				}
			}
		}
	}
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// latestURL 是最新发布版本的页面，GitHub 把它重定向到 .../releases/tag/<版本>（不含预发布版）。
// 用重定向而不是 GitHub API：不占 API 的限额（每个 IP 每小时 60 次）。
var latestURL = "https://github.com/" + Repo + "/releases/latest"

// Latest 返回最新发布的版本号。直接连不上 GitHub 时经加速代理再查（代理也转发重定向）：先用 proxy（记着的代理，可以为空），
// 再依次用 GitHubProxies 里的其他代理。都查不到时返回直接查询的错误。
//
// via 是查到版本的加速代理（主机名），直接从 GitHub 查到时为空。经代理查到的版本号由代理决定，可能是 GitHub 上没有的
// 版本：界面写明「查询自 <代理>」；下载时能连上 GitHub 就按 GitHub 上这个版本的 SHA256SUMS 核对，GitHub 上没有
// 这个版本时不安装（见 fetchFirst）。节点升级的版本是面板自己正在运行的版本，不是这里查到的，所以这个版本号不会直接下发给节点。
func Latest(ctx context.Context, proxy string) (version, via string, err error) {
	v, err := latest(ctx, latestURL, 15*time.Second)
	if err == nil {
		return v, "", nil
	}
	tried := map[string]bool{}
	for _, p := range append([]string{proxy}, knownProxies...) {
		if p == "" || tried[p] || ctx.Err() != nil {
			continue
		}
		tried[p] = true
		if pv, perr := latest(ctx, p+latestURL, 10*time.Second); perr == nil {
			return pv, hostOf(p), nil
		}
	}
	return "", "", err
}

func latest(ctx context.Context, url string, timeout time.Duration) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout:       timeout,
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
