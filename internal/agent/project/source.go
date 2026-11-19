package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/net/proxy"
)

func (p containerPaths) sshDir() string       { return filepath.Join(p.root, "ssh") }
func (p containerPaths) deployKey() string    { return filepath.Join(p.sshDir(), "deploy_key") }
func (p containerPaths) knownHosts() string   { return filepath.Join(p.sshDir(), "known_hosts") }
func (p containerPaths) checkoutDir() string  { return filepath.Join(p.root, ".checkout") }
func (p containerPaths) deployKeyPub() string { return p.deployKey() + ".pub" }

// gitAuth 返回访问仓库用的认证：SSH 地址用容器的 deploy key（面板生成，部署时写进容器目录），HTTPS 地址不认证（公开仓库）。
func gitAuth(p containerPaths, url string) (*gitssh.PublicKeys, error) {
	if !spec.IsSSHURL(url) {
		return nil, nil
	}
	key, err := os.ReadFile(p.deployKey())
	if err != nil {
		return nil, errors.New("容器还没有 deploy key")
	}
	auth, err := gitssh.NewPublicKeys("git", key, "")
	if err != nil {
		return nil, err
	}
	auth.HostKeyCallback = trustOnFirstUse(p.knownHosts())
	return auth, nil
}

// trustOnFirstUse：第一次连接某个 git 服务器时记住它的主机密钥，之后密钥变了就拒绝，防止被冒充。
func trustOnFirstUse(file string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if _, err := os.Stat(file); err == nil {
			cb, err := knownhosts.New(file)
			if err != nil {
				return err
			}
			err = cb(hostname, remote, key)
			var keyErr *knownhosts.KeyError
			if err == nil {
				return nil
			}
			if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
				return fmt.Errorf("git 服务器 %s 的主机密钥和之前记录的不一致，可能被冒充；如果服务器确实更换了密钥，请删除 %s", hostname, file)
			}
		}
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
		return err
	}
}

// git 地址是操作员填的，拉取在节点上进行，不能借此访问节点上只监听本机的服务（network_mode: host 仅限管理员）。
// 所以在域名解析之后检查实际连接的地址（spec.BlockedGitIP），重定向也一样。局域网地址照常允许（私有 git 服务器）。
// HTTP(S) 换掉 go-git 的全局客户端（本包以外没有用 go-git）；SSH 没有全局入口，每次拉取用登记过的代理类型接管连接。
const guardProxyScheme = "nagi-git-guard"

var installGuardOnce sync.Once

func guardedDialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: checkGitAddr}
}

func checkGitAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if spec.BlockedGitIP(ip) {
		return fmt.Errorf("git 服务器的地址 %s 是节点本机或保留地址，已拒绝连接", host)
	}
	return nil
}

// guardClone 让这次拉取只连接允许的地址。
func guardClone(opts *git.CloneOptions) {
	installGuardOnce.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = guardedDialer().DialContext
		c := githttp.NewClient(&http.Client{Transport: tr})
		client.InstallProtocol("http", c)
		client.InstallProtocol("https", c)
		proxy.RegisterDialerType(guardProxyScheme, func(*url.URL, proxy.Dialer) (proxy.Dialer, error) {
			return guardedDialer(), nil
		})
	})
	if spec.IsSSHURL(opts.URL) {
		opts.ProxyOptions = transport.ProxyOptions{URL: guardProxyScheme + "://"}
	}
}

// maxProgressLine 是 git 进度输出一行最多转发多少字节，超过的截断。
const maxProgressLine = 4 << 10

// lineWriter 把 git 的进度输出按行转发。进度是 git 服务器发来的：只在新写入的部分找换行，一行超过 maxProgressLine
// 就截断发出、丢掉到换行为止的其余部分。不换行的数据不会让缓冲一直变大、每次从头重新扫描（v0.35.0#21）。
type lineWriter struct {
	onLine func(string)
	buf    []byte
	skip   bool // 这一行已经截断发出，丢掉到换行为止的部分
}

func (w *lineWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexAny(b, "\r\n")
		if i < 0 {
			w.add(b)
			break
		}
		w.add(b[:i])
		if !w.skip {
			w.emit(false)
		}
		w.buf, w.skip = w.buf[:0], false
		b = b[i+1:]
	}
	return n, nil
}

// add 把一行的一部分放进缓冲，满了 maxProgressLine 就截断发出。
func (w *lineWriter) add(b []byte) {
	if w.skip {
		return
	}
	if room := maxProgressLine - len(w.buf); len(b) >= room {
		w.buf = append(w.buf, b[:room]...)
		w.emit(true)
		w.buf, w.skip = w.buf[:0], true
		return
	}
	w.buf = append(w.buf, b...)
}

func (w *lineWriter) emit(clipped bool) {
	line := strings.TrimSpace(strings.ToValidUTF8(string(w.buf), ""))
	if line == "" {
		return
	}
	if clipped {
		line += "…"
	}
	w.onLine(line)
}

// gitPullTimeout 是从 git 拉取的最长时间（和拉取镜像一样，文案里写的是 30 分钟，改的话一起改）。
var gitPullTimeout = 30 * time.Minute

// gitDiskEvery 是拉取期间多久检查一次磁盘剩余空间：go-git 自己写文件，没法每次写入前检查，就定时查。
var gitDiskEvery = 2 * time.Second

// checkDisk 检查磁盘剩余空间，测试时替换。
var checkDisk = metrics.CheckDisk

// guardedClone 运行 clone，开始前和进行中定时检查 dir 所在磁盘的剩余空间，不够了就取消：恶意的 git 服务器
// 发一个无穷大的 packfile 时不会写满磁盘（v0.35.0#21）。总时长由调用方的 ctx 限制（见 pull）。
func guardedClone(ctx context.Context, dir string, clone func(ctx context.Context) error) error {
	if err := checkDisk(dir, 0); err != nil {
		return err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var low error
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		t := time.NewTicker(gitDiskEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-cctx.Done():
				return
			case <-t.C:
				if err := checkDisk(dir, 0); err != nil {
					low = err
					cancel()
					return
				}
			}
		}
	}()
	err := clone(cctx)
	close(stop)
	<-watched // 之后读 low 不用加锁
	if err != nil && low != nil {
		return low
	}
	return err
}

// pull 从 git 拉取文件替换容器的 data/，保留 preserve 里的路径。返回拉取到的提交。
func pull(ctx context.Context, p containerPaths, src *nagiv1.Source, preserve []string, onLine func(string)) (string, error) {
	url := strings.TrimSpace(src.GetGitUrl())
	if url == "" {
		return "", invalid("容器没有设置 git 仓库")
	}
	auth, err := gitAuth(p, url)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(p.checkoutDir()); err != nil {
		return "", err
	}
	defer os.RemoveAll(p.checkoutDir())

	onLine("拉取 " + spec.RedactGitURL(url))
	opts := &git.CloneOptions{
		URL:          url,
		Depth:        1,
		SingleBranch: true,
		Progress:     &lineWriter{onLine: func(l string) { onLine("  " + l) }},
	}
	if auth != nil {
		opts.Auth = auth
	}
	guardClone(opts)
	// 拉取（包括按标签再试一次）的总时长有上限：恶意的 git 服务器一直慢慢发数据时，不会一直占着容器的锁（v0.35.0#21）。
	pctx, cancel := context.WithTimeout(ctx, gitPullTimeout)
	defer cancel()
	ref := strings.TrimSpace(src.GetGitRef())
	clone := func(name plumbing.ReferenceName) (repo *git.Repository, err error) {
		os.RemoveAll(p.checkoutDir())
		opts.ReferenceName = name
		err = guardedClone(pctx, p.root, func(ctx context.Context) (err error) {
			repo, err = git.PlainCloneContext(ctx, p.checkoutDir(), false, opts)
			return err
		})
		return repo, err
	}
	var repo *git.Repository
	if ref == "" {
		repo, err = clone("")
	} else if repo, err = clone(plumbing.NewBranchReferenceName(ref)); err != nil {
		// 不是分支，再按标签试一次。
		repo, err = clone(plumbing.NewTagReferenceName(ref))
	}
	var low *metrics.LowDiskError
	switch {
	case err == nil:
	case ctx.Err() == nil && errors.Is(pctx.Err(), context.DeadlineExceeded):
		return "", errors.New("拉取超过 30 分钟，已停止")
	case errors.As(err, &low):
		return "", err
	}
	if err != nil {
		if spec.IsSSHURL(url) {
			return "", fmt.Errorf("拉取失败：%w（请确认已把容器的 deploy key 加到仓库）", err)
		}
		return "", fmt.Errorf("拉取失败：%w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	commit := head.Hash().String()
	onLine("拉取到提交 " + commit[:12])

	if err := os.RemoveAll(filepath.Join(p.checkoutDir(), ".git")); err != nil {
		return "", err
	}
	if err := syncDir(p.checkoutDir(), p.dataDir(), preserve, onLine); err != nil {
		return "", fmt.Errorf("更新文件: %w", err)
	}
	return commit, nil
}

// preserved 判断相对路径是否属于保留路径（等于某个保留路径或在它里面）。
func preserved(rel string, preserve []string) bool {
	for _, p := range preserve {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// containsPreserved 判断某个目录里面是否有保留路径。这样的目录不能整个删掉。
func containsPreserved(rel string, preserve []string) bool {
	for _, p := range preserve {
		if strings.HasPrefix(p, rel+"/") {
			return true
		}
	}
	return false
}

// copyFile 复制一个文件。只用于容器碰不到的目录；写进 data/、volumes/ 要用 safefs.go 里的函数。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
