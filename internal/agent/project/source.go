package project

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func (p containerPaths) sshDir() string       { return filepath.Join(p.root, "ssh") }
func (p containerPaths) deployKey() string    { return filepath.Join(p.sshDir(), "deploy_key") }
func (p containerPaths) knownHosts() string   { return filepath.Join(p.sshDir(), "known_hosts") }
func (p containerPaths) checkoutDir() string  { return filepath.Join(p.root, ".checkout") }
func (p containerPaths) deployKeyPub() string { return p.deployKey() + ".pub" }

// ensureDeployKey 返回容器的 deploy key 公钥（authorized_keys 格式），不存在时生成一对 ed25519 密钥。
func ensureDeployKey(p containerPaths) (string, error) {
	if pub, err := os.ReadFile(p.deployKeyPub()); err == nil {
		return strings.TrimSpace(string(pub)), nil
	}
	if err := os.MkdirAll(p.sshDir(), 0o700); err != nil {
		return "", err
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(privKey, "nagi deploy key")
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(p.deployKey(), pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", err
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " nagi"
	if err := writeFileAtomic(p.deployKeyPub(), []byte(pub+"\n"), 0o644); err != nil {
		return "", err
	}
	return pub, nil
}

func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "ssh://") || (strings.Contains(url, "@") && !strings.Contains(url, "://"))
}

// gitAuth 返回访问仓库用的认证：SSH 地址用容器的 deploy key，HTTPS 地址不认证（公开仓库）。
func gitAuth(p containerPaths, url string) (*gitssh.PublicKeys, error) {
	if !isSSHURL(url) {
		return nil, nil
	}
	if _, err := ensureDeployKey(p); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(p.deployKey())
	if err != nil {
		return nil, err
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

// lineWriter 把 git 的进度输出按行转发。
type lineWriter struct {
	onLine func(string)
	buf    []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := strings.IndexAny(string(w.buf), "\r\n")
		if i < 0 {
			return len(b), nil
		}
		if line := strings.TrimSpace(string(w.buf[:i])); line != "" {
			w.onLine(line)
		}
		w.buf = w.buf[i+1:]
	}
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

	onLine("拉取 " + url)
	opts := &git.CloneOptions{
		URL:          url,
		Depth:        1,
		SingleBranch: true,
		Progress:     &lineWriter{onLine: func(l string) { onLine("  " + l) }},
	}
	if auth != nil {
		opts.Auth = auth
	}
	ref := strings.TrimSpace(src.GetGitRef())
	clone := func(name plumbing.ReferenceName) (*git.Repository, error) {
		os.RemoveAll(p.checkoutDir())
		opts.ReferenceName = name
		return git.PlainCloneContext(ctx, p.checkoutDir(), false, opts)
	}
	var repo *git.Repository
	if ref == "" {
		repo, err = clone("")
	} else if repo, err = clone(plumbing.NewBranchReferenceName(ref)); err != nil {
		// 不是分支，再按标签试一次。
		repo, err = clone(plumbing.NewTagReferenceName(ref))
	}
	if err != nil {
		if isSSHURL(url) {
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

// normalizePreserve 把保留路径规范成相对 data/ 的 "a/b" 形式，去掉不合法的。
func normalizePreserve(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range list {
		p = path.Clean(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")))
		p = strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
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

// bindMountPaths 返回 compose 里挂载进容器的、位于 data/ 下的路径（相对 data/）。
// 这些路径自动保留，防止更新文件时把数据库目录之类的运行数据冲掉。
func bindMountPaths(proj *types.Project, dataDir string) []string {
	var out []string
	for _, svc := range proj.Services {
		for _, v := range svc.Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			rel, err := filepath.Rel(dataDir, v.Source)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				continue
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// dataServices 返回挂载了 data/ 里文件的服务。文件更新后要重建这些服务的容器，
// 因为 compose 在配置没变时不会重建容器。
func dataServices(proj *types.Project, dataDir string) []string {
	var out []string
	for _, name := range proj.ServiceNames() {
		for _, v := range proj.Services[name].Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			rel, err := filepath.Rel(dataDir, v.Source)
			if err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}
