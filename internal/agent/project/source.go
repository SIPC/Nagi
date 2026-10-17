package project

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

func (p paths) sshDir() string       { return filepath.Join(p.configDir(), "ssh") }
func (p paths) deployKey() string    { return filepath.Join(p.sshDir(), "deploy_key") }
func (p paths) knownHosts() string   { return filepath.Join(p.sshDir(), "known_hosts") }
func (p paths) checkoutDir() string  { return filepath.Join(p.configDir(), ".checkout") }
func (p paths) deployKeyPub() string { return p.deployKey() + ".pub" }

// ensureDeployKey 返回项目的 deploy key 公钥（authorized_keys 格式），不存在时生成一对 ed25519 密钥。
func ensureDeployKey(p paths) (string, error) {
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

// gitAuth 返回访问仓库用的认证：SSH 地址用项目的 deploy key，HTTPS 地址不认证（公开仓库）。
func gitAuth(p paths, url string) (*gitssh.PublicKeys, error) {
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

// pull 从 git 拉取文件替换 data/，保留 preserve 里的路径。返回拉取到的提交。
func pull(ctx context.Context, p paths, src *nagiv1.Source, preserve []string, onLine func(string)) (string, error) {
	url := strings.TrimSpace(src.GetGitUrl())
	if url == "" {
		return "", invalid("项目没有设置 git 仓库")
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
			return "", fmt.Errorf("拉取失败：%w（请确认已把项目的 deploy key 加到仓库）", err)
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

// syncDir 让 dst 变成 src 的内容：删掉 dst 里不在保留路径中的所有东西，再复制 src。
// 保留路径在 dst 里已经存在时，src 里的这部分被忽略，以 dst 现有内容为准；
// 还不存在时（例如第一次部署），用 src 里的内容初始化。
func syncDir(src, dst string, preserve []string, onLine func(string)) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// 1. 清理旧文件
	err := filepath.WalkDir(dst, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dst, full)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if preserved(rel, preserve) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && containsPreserved(rel, preserve) {
			return nil
		}
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 2. 复制新文件。节点上还没有的保留路径不算保留，照常复制。
	var existing []string
	for _, p := range preserve {
		if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(p))); err == nil {
			existing = append(existing, p)
		}
	}
	preserve = existing
	ignored := map[string]bool{}
	return filepath.WalkDir(src, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, full)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if preserved(rel, preserve) {
			root := rel
			for _, p := range preserve {
				if rel == p || strings.HasPrefix(rel, p+"/") {
					root = p
				}
			}
			if !ignored[root] {
				ignored[root] = true
				onLine("保留路径 " + root + " 以节点上现有内容为准，新文件里的这部分被忽略")
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(full)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(full, target, info.Mode().Perm())
		}
	})
}

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
