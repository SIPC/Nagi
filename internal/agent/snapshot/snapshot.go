// Package snapshot 用内嵌的 kopia 给项目做快照。每个项目一个 kopia 仓库，放在项目目录的 backups/ 下，
// 增量、去重、加密。仓库密码随机生成，存在项目的 config/ 里；连接配置和缓存放在节点目录，不随项目走。
package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
)

// Kind 是快照的种类，每种是一个独立的快照来源，各自有保留策略。
type Kind string

const (
	// Scheduled 定时快照：data/、volumes/、config/。保留最近 24 小时每小时一份、7 天每天一份、4 周每周一份。
	Scheduled Kind = "scheduled"
	// Manual 手动快照，内容同定时快照。保留最近 20 份。
	Manual Kind = "manual"
	// PreDeploy 部署前的运行数据：volumes/ 和 data/（恢复时只用保留路径）。保留最近 20 份。
	PreDeploy Kind = "predeploy"
	// Version 版本文件：data/ 中除保留路径以外的内容。保留最近 20 份。
	Version Kind = "version"
)

func intp(v int) *policy.OptionalInt {
	o := policy.OptionalInt(v)
	return &o
}

func retention(kind Kind) policy.RetentionPolicy {
	if kind == Scheduled {
		return policy.RetentionPolicy{
			KeepLatest: intp(1), KeepHourly: intp(24), KeepDaily: intp(7), KeepWeekly: intp(4),
			KeepMonthly: intp(0), KeepAnnual: intp(0),
		}
	}
	return policy.RetentionPolicy{
		KeepLatest: intp(20), KeepHourly: intp(0), KeepDaily: intp(0), KeepWeekly: intp(0),
		KeepMonthly: intp(0), KeepAnnual: intp(0),
	}
}

// Repo 是一个项目的快照仓库。
type Repo struct {
	rep       repo.Repository
	projectID string
}

// Open 打开项目的快照仓库，不存在时创建。
//   - storageDir：项目目录的 backups/
//   - passwordFile：项目目录里存仓库密码的文件，不存在时生成
//   - stateDir：节点目录下这个项目的连接配置和缓存
func Open(ctx context.Context, projectID, storageDir, passwordFile, stateDir string) (*Repo, error) {
	password, err := loadPassword(passwordFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	configFile := filepath.Join(stateDir, "repository.config")
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		if err := connect(ctx, storageDir, configFile, filepath.Join(stateDir, "cache"), password); err != nil {
			return nil, err
		}
	}
	rep, err := repo.Open(ctx, configFile, password, &repo.Options{})
	if err != nil {
		// 项目目录被移动过等情况，连接配置失效，重新连接一次。
		os.Remove(configFile)
		if err := connect(ctx, storageDir, configFile, filepath.Join(stateDir, "cache"), password); err != nil {
			return nil, err
		}
		if rep, err = repo.Open(ctx, configFile, password, &repo.Options{}); err != nil {
			return nil, fmt.Errorf("打开快照仓库: %w", err)
		}
	}
	return &Repo{rep: rep, projectID: projectID}, nil
}

func connect(ctx context.Context, storageDir, configFile, cacheDir, password string) error {
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return err
	}
	st, err := filesystem.New(ctx, &filesystem.Options{Path: storageDir}, true)
	if err != nil {
		return err
	}
	defer st.Close(ctx)
	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil && !errors.Is(err, repo.ErrAlreadyInitialized) {
		return fmt.Errorf("创建快照仓库: %w", err)
	}
	return repo.Connect(ctx, configFile, st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: cacheDir},
	})
}

func loadPassword(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return "", err
	}
	return pw, nil
}

// Close 关闭仓库。
func (r *Repo) Close(ctx context.Context) error { return r.rep.Close(ctx) }

func (r *Repo) source(kind Kind) snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: "nagi", UserName: r.projectID, Path: "/" + string(kind)}
}

// Dir 是快照里的一个顶层目录：Name 是快照里的名字，Path 是节点上的实际路径。
type Dir struct {
	Name string
	Path string
}

// Create 给几个目录做一份快照。ignore 是忽略规则（gitignore 语法，相对快照根目录，例如 /data/uploads/）。
// 返回快照 ID。做完后按这一种快照的保留策略清理旧快照。
func (r *Repo) Create(ctx context.Context, kind Kind, dirs []Dir, ignore []string, tags map[string]string) (string, error) {
	id, _, err := r.CreateWithRoot(ctx, kind, dirs, ignore, tags)
	return id, err
}

// CreateWithRoot 同 Create，另外返回快照内容的根对象 ID：内容完全相同的两份快照，根对象 ID 相同，
// 可以用来判断文件有没有变化。
func (r *Repo) CreateWithRoot(ctx context.Context, kind Kind, dirs []Dir, ignore []string, tags map[string]string) (string, string, error) {
	var entries []fs.Entry
	for _, d := range dirs {
		if err := os.MkdirAll(d.Path, 0o755); err != nil {
			return "", "", err
		}
		dir, err := localfs.Directory(d.Path)
		if err != nil {
			return "", "", err
		}
		entries = append(entries, renamed{Directory: dir, name: d.Name})
	}
	root := virtualfs.NewStaticDirectory("root", entries)

	si := r.source(kind)
	pol := &policy.Policy{
		RetentionPolicy: retention(kind),
		FilesPolicy:     policy.FilesPolicy{IgnoreRules: ignore},
	}
	tree := policy.BuildTree(map[string]*policy.Policy{".": pol}, policy.DefaultPolicy)

	var id manifest.ID
	var rootID string
	err := repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "nagi snapshot"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		prev, err := snapshot.ListSnapshots(ctx, w, si)
		if err != nil {
			return err
		}
		sort.Slice(prev, func(i, j int) bool { return prev[i].StartTime.After(prev[j].StartTime) })
		if len(prev) > 1 {
			prev = prev[:1]
		}
		man, err := upload.NewUploader(w).Upload(ctx, root, tree, si, prev...)
		if err != nil {
			return err
		}
		if man.IncompleteReason != "" {
			return fmt.Errorf("快照未完成：%s", man.IncompleteReason)
		}
		if man.RootEntry != nil {
			rootID = man.RootEntry.ObjectID.String()
		}
		man.Tags = map[string]string{"kind": string(kind)}
		for k, v := range tags {
			man.Tags[k] = v
		}
		if id, err = snapshot.SaveSnapshot(ctx, w, man); err != nil {
			return err
		}
		if err := policy.SetPolicy(ctx, w, si, pol); err != nil {
			return err
		}
		_, err = policy.ApplyRetentionPolicy(ctx, w, si, true)
		return err
	})
	return string(id), rootID, err
}

// Info 是快照的摘要信息。
type Info struct {
	ID   string
	Kind Kind
	Time time.Time
	Size int64
	Tags map[string]string
}

// List 列出某一种快照，最新的在前。kind 为空时列出全部。
func (r *Repo) List(ctx context.Context, kinds ...Kind) ([]Info, error) {
	if len(kinds) == 0 {
		kinds = []Kind{Scheduled, Manual, PreDeploy, Version}
	}
	var out []Info
	for _, k := range kinds {
		mans, err := snapshot.ListSnapshots(ctx, r.rep, r.source(k))
		if err != nil {
			return nil, err
		}
		for _, m := range mans {
			info := Info{ID: string(m.ID), Kind: k, Time: m.StartTime.ToTime(), Tags: m.Tags}
			if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
				info.Size = m.RootEntry.DirSummary.TotalFileSize
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Restore 把快照里名为 name 的顶层目录恢复到 target。target 里多余的文件会被删除，
// 所以调用方要先决定好恢复到哪里（例如先恢复到临时目录，再按保留路径合并）。
func (r *Repo) Restore(ctx context.Context, id, name, target string) error {
	man, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if err != nil {
		return fmt.Errorf("读取快照: %w", err)
	}
	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return err
	}
	dir, ok := root.(fs.Directory)
	if !ok {
		return errors.New("快照根目录无效")
	}
	child, err := dir.Child(ctx, name)
	if err != nil {
		return fmt.Errorf("快照里没有 %s: %w", name, err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	out := &restore.FilesystemOutput{
		TargetPath:             target,
		OverwriteDirectories:   true,
		OverwriteFiles:         true,
		OverwriteSymlinks:      true,
		IgnorePermissionErrors: true,
	}
	if err := out.Init(ctx); err != nil {
		return err
	}
	// RestoreDirEntryAtDepth 默认 0 表示"浅恢复"（只生成占位文件），这里要完整恢复。
	_, err = restore.Entry(ctx, r.rep, out, child, restore.Options{Parallel: 4, DeleteExtra: true, RestoreDirEntryAtDepth: math.MaxInt32})
	return err
}

// Tags 返回快照的标签。
func (r *Repo) Tags(ctx context.Context, id string) (map[string]string, error) {
	man, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if err != nil {
		return nil, err
	}
	return man.Tags, nil
}

// renamed 让本地目录在快照里换个名字（例如 /mnt/qmt/data 在快照里叫 data）。
type renamed struct {
	fs.Directory
	name string
}

func (r renamed) Name() string { return r.name }
