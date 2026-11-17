package release

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// GitHubProxies 是下载 GitHub 上的文件时用的加速代理（地址前缀，代理地址后面接 GitHub 上的完整地址），空格分隔，按顺序试。
// 和安装脚本里的 GH_PROXIES 一致（internal/server/api 的 TestInstallConstants 检查）。都是第三方服务，可能失效。
const GitHubProxies = "https://ghfast.top/ https://gh-proxy.com/ https://ghproxy.net/"

// knownProxies 是 GitHubProxies 拆开的列表，测试里换掉，免得连到外面。
var knownProxies = strings.Fields(GitHubProxies)

// downloadTimeout 是下载新版本（所有地址加起来）的时间上限。一个地址太慢（见 fetch）就换下一个，不把时间都耗在它上面。
const downloadTimeout = 10 * time.Minute

// Source 是下载新版本的一个地址。
type Source struct {
	// Base 下有 nagi-linux-<架构> 和 SHA256SUMS。
	Base string
	// Proxy 是经 GitHub 加速代理下载时代理的前缀（https://…/），直接从 GitHub 下载和用镜像时为空。
	Proxy string
	// Mirror：安装时用 --download-url 指定的镜像。
	Mirror bool
}

// Name 是给人看的地址：主机名，例如 github.com、ghfast.top。
func (s Source) Name() string {
	if s.Proxy != "" {
		return hostOf(s.Proxy)
	}
	return hostOf(s.Base)
}

func (s Source) direct() bool { return !s.Mirror && s.Proxy == "" }

// Sources 返回 dir 对应的安装下载 version 时依次尝试的地址：
//   - 安装时指定了镜像（download-url）就只用镜像，和以前一样；
//   - 否则先用记下的加速代理（github-proxy），再直接从 GitHub 下载，最后依次试 GitHubProxies 里的其他代理。
//     不论顺序，能连上 GitHub 时各代理的文件都按 GitHub 上的 SHA256SUMS 核对（见 fetchFirst）。
func Sources(dir, version string) []Source {
	if m := mirrorFor(dir); m != "" {
		return []Source{{Base: m, Mirror: true}}
	}
	direct := DownloadBase(version)
	var out []Source
	seen := map[string]bool{}
	add := func(proxy string) {
		if seen[proxy] {
			return
		}
		seen[proxy] = true
		out = append(out, Source{Base: proxy + direct, Proxy: proxy})
	}
	if p := ProxyFor(dir); p != "" {
		add(p)
	}
	add("")
	for _, p := range knownProxies {
		add(p)
	}
	return out
}

func mirrorFor(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, downloadURLFile))
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "/")
}

// remember 记下这次用上的地址，下次先试它：经代理下载的写进 github-proxy，直接从 GitHub 下载的删掉 github-proxy。
// 用镜像时什么都不改。
func remember(dir string, s Source) error {
	if s.Mirror {
		return nil
	}
	f := filepath.Join(dir, githubProxyFile)
	if s.Proxy == "" {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if ProxyFor(dir) == s.Proxy {
		return nil
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, []byte(s.Proxy+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, f); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// pinTimeout 是开始下载之前直接从 GitHub 取 SHA256SUMS 的时间上限（见 githubSums）。测试里改短。
var pinTimeout = 15 * time.Second

// githubSums 在开始下载之前，直接从 GitHub（sources 里直接下载的那个地址）取 SHA256SUMS（64 KB 以内），
// 返回本机架构的文件应有的 SHA-256。sources 里没有加速代理时不用取，返回空。
//   - 取到了：之后每个地址（包括排在最前、记着的加速代理）提供的 SHA256SUMS 都必须和它一致；
//   - GitHub 上没有这个版本（HTTP 404），或者 SHA256SUMS 里没有本机的架构：返回错误，不再试加速代理。
//     加速代理可以谎报一个 GitHub 上没有的新版本（见 Latest），再提供自己的文件和校验值，这样的版本不能装；
//   - 连不上 GitHub（超时、连接失败、其他 HTTP 状态）：返回空，各地址按自己的 SHA256SUMS 核对。
//     完全连不上 GitHub 时只能信任加速代理，这是已接受的风险。
func githubSums(ctx context.Context, sources []Source) (string, error) {
	var direct *Source
	proxied := false
	for i, s := range sources {
		if s.direct() && direct == nil {
			direct = &sources[i]
		}
		proxied = proxied || s.Proxy != ""
	}
	if direct == nil || !proxied {
		return "", nil
	}
	pctx, cancel := context.WithTimeout(ctx, pinTimeout)
	defer cancel()
	b, err := fetch(pctx, direct.Base+"/SHA256SUMS", 64<<10, nil)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	version := path.Base(direct.Base)
	var se statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
		return "", i18n.Errorf("GitHub 上没有版本 %s，已停止下载", version)
	case err != nil:
		return "", nil
	}
	name := binaryName()
	sum := Checksum(string(b), name)
	if sum == "" {
		return "", i18n.Errorf("GitHub 上的 %s 没有 %s，已停止下载", version, name)
	}
	return sum, nil
}

// fetchFirst 按顺序从 sources 下载新版本到 dst，每个地址的文件都按同一地址下的 SHA256SUMS 核对，再用 check 检查
// （试运行；check 为空时不检查）。返回第一个成功的地址。
//
// 开始之前先直接从 GitHub 取 SHA256SUMS（见 githubSums）。取到了，经加速代理下载的 SHA256SUMS 里的值必须和它一致，
// 记着的、排在 GitHub 前面的代理也一样：代理不能换一份自己的文件和校验值。GitHub 上没有这个版本时直接失败。
// 这一次没取到、之后直接下载时取到了（即使文件下载失败），后面的代理也要和它一致。
func fetchFirst(ctx context.Context, sources []Source, dst string, check func(Source) error, log *slog.Logger) (Source, error) {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	pinned, err := githubSums(ctx, sources)
	if err != nil {
		return Source{}, err
	}
	var first error
	for _, s := range sources {
		pin := ""
		if s.Proxy != "" {
			pin = pinned
		}
		sum, err := download(ctx, s.Base, dst, pin)
		if err == nil && check != nil {
			err = check(s)
		}
		if err == nil {
			return s, nil
		}
		if s.direct() && sum != "" && pinned == "" {
			pinned = sum
		}
		if ctx.Err() != nil {
			return Source{}, ctx.Err()
		}
		if IsUnusable(err) {
			// 文件已经核对过，换个地址下载到的也是它。
			return Source{}, err
		}
		if len(sources) > 1 {
			log.Warn("从这个地址下载新版本失败，换下一个", "source", s.Name(), "err", err)
		}
		if first == nil {
			first = err
		}
	}
	if len(sources) == 1 {
		return Source{}, first
	}
	return Source{}, i18n.Errorf("已尝试 %d 个下载地址，均未成功（%s：%w）", len(sources), sources[0].Name(), first)
}
