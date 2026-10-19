package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// 容器能写自己的 data/ 和 volumes/，里面随时可能出现指向宿主机任意位置的符号链接（例如被攻破的容器
// 把 data/conf 换成指向 /etc 的链接）。Nagi 以 root 运行，在这些目录里写、删文件时如果跟着链接走，
// 容器就能借 Nagi 改宿主机的文件。所以这里的操作一律经 os.Root：路径解析不会走出根目录，
// 指向外面的链接会直接报错，而且是逐级打开，不怕检查之后链接被换掉。

// openDirRoot 打开（必要时创建）目录 dir 作为根。dir 本身是符号链接时拒绝。
func openDirRoot(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(dir); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s 是符号链接，拒绝写入", dir)
	}
	return os.OpenRoot(dir)
}

// syncDir 让 dst 变成 src 的内容：删掉 dst 里不在保留路径中的所有东西，再复制 src。
// 保留路径在 dst 里已经存在时，src 里的这部分被忽略，以 dst 现有内容为准；
// 还不存在时（例如第一次部署），用 src 里的内容初始化。
// src 必须是容器碰不到的目录（检出目录、上传目录、恢复用的临时目录）；dst 经 os.Root 操作。
func syncDir(src, dst string, preserve []string, onLine func(string)) error {
	root, err := openDirRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	return syncRoot(root, src, preserve, onLine)
}

// syncRoot 和 syncDir 一样，目标是已经打开的根。
func syncRoot(root *os.Root, src string, preserve []string, onLine func(string)) error {
	// 1. 清理旧文件。WalkDir 不跟随符号链接，链接本身会被删掉。
	err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
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
		if err := root.RemoveAll(rel); err != nil {
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
		if _, err := root.Lstat(p); err == nil {
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
		if preserved(rel, preserve) {
			top := rel
			for _, p := range preserve {
				if rel == p || strings.HasPrefix(rel, p+"/") {
					top = p
				}
			}
			if !ignored[top] {
				ignored[top] = true
				onLine("保留路径 " + top + " 以节点上现有内容为准，新文件里的这部分被忽略")
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
			return root.MkdirAll(rel, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(full)
			if err != nil {
				return err
			}
			return root.Symlink(link, rel)
		default:
			return copyFileInto(root, rel, full, info.Mode().Perm())
		}
	})
}

// copyFileInto 把 src 复制到根 root 下的 rel，需要时创建上级目录。
func copyFileInto(root *os.Root, rel, src string, mode os.FileMode) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// checkMountSources 检查 compose 里绑定挂载的、位于容器目录下的源路径（例如 ./uploads，即 data/uploads）。
// 源路径如果被容器换成了指向外面的符号链接，Docker 挂载时会跟着链接走，把宿主机的目录挂进容器。
func checkMountSources(proj *types.Project, base string) error {
	for _, svc := range proj.Services {
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind {
				if err := noSymlinkBelow(base, v.Source); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// noSymlinkBelow 检查 path 在 base 之下的每一级都不是符号链接（不存在的部分不算）。path 不在 base 下时不检查。
func noSymlinkBelow(base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return invalid("%s 是符号链接（可能是容器自己创建的），为了安全不挂载它。删掉这个链接后重新部署", cur)
		}
	}
	return nil
}

// removeSymlinks 删掉 dir 下所有的符号链接，在把快照原地恢复到 data/、volumes/ 之前调用（容器已经停止）：
// 恢复时会覆盖已有的文件，目标位置是容器留下的链接的话，内容会写到链接指向的地方。
// 快照里本来就有的链接会被恢复回来。
func removeSymlinks(dir string) error {
	root, err := openDirRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return root.Remove(rel)
		}
		return nil
	})
}
