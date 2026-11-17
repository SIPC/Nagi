package release

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

// fetchFirst 按顺序从 sources 下载新版本到 dst，每个地址的文件都按同一地址下的 SHA256SUMS 核对，再用 check 检查
// （试运行；check 为空时不检查）。返回第一个成功的地址。
//
// 直接从 GitHub 拿到了 SHA256SUMS（即使后面下载文件失败）时，之后经加速代理下载的 SHA256SUMS 里的值必须和它一致：
// 代理不能借直接下载太慢换一份自己的文件和校验值。
func fetchFirst(ctx context.Context, sources []Source, dst string, check func(Source) error, log *slog.Logger) (Source, error) {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	var pinned string
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
		if s.direct() && sum != "" {
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
