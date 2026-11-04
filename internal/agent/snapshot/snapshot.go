// Package snapshot 用内嵌的 kopia 给容器做快照。每个容器一个 kopia 仓库，放在容器目录的 backups/ 下，
// 增量、去重、加密。仓库密码由面板生成和保存，随配置下发；连接配置和缓存放在节点目录，不随容器走。
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"math"
	"os"
	"path"
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
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
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

// Repo 是一个容器的快照仓库。
type Repo struct {
	rep   repo.Repository
	owner string
	// Limit 是定时、手动、部署前快照合计最多保留的份数，0 表示不限。每次做完这三种快照后按它删除最老的。
	Limit int
}

// limited 是计入份数上限、可以手动删除的快照种类。版本文件不算，回滚要用。
var limited = []Kind{Scheduled, Manual, PreDeploy}

// ErrNotDeletable 表示这份快照不能删除（版本文件）。
var ErrNotDeletable = errors.New("snapshot not deletable")

// Deletable 判断这种快照能不能删除。
func Deletable(k Kind) bool { return k == Scheduled || k == Manual || k == PreDeploy }

// Open 打开容器的快照仓库，不存在时创建。
//   - owner：快照来源里的用户名，例如 <项目 ID>-<容器名>
//   - storageDir：容器目录的 backups/
//   - password：仓库密码
//   - stateDir：节点目录下这个仓库的连接配置和缓存
func Open(ctx context.Context, owner, storageDir, password, stateDir string) (*Repo, error) {
	if password == "" {
		return nil, errors.New("缺少快照仓库密码")
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
	return &Repo{rep: rep, owner: owner}, nil
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

// Close 关闭仓库。
func (r *Repo) Close(ctx context.Context) error { return r.rep.Close(ctx) }

func (r *Repo) source(kind Kind) snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: "nagi", UserName: r.owner, Path: "/" + string(kind)}
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
	// 失败（超时、取消）时也提交已经写好的内容：下次重试直接复用，不会每次重新上传、留下一批新的数据包
	// （见 docs/snapshot.md#失败与退避）。会话用不随调用方取消的 ctx，否则超时以后提交也会失败；上传和读写记录用调用方的 ctx。
	// 快照记录和保留规则在上传完整以后才写，所以失败时不会留下不完整的快照。
	err := repo.WriteSession(context.WithoutCancel(ctx), r.rep, repo.WriteSessionOptions{Purpose: "nagi snapshot", FlushOnFailure: true}, func(_ context.Context, w repo.RepositoryWriter) error {
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
		if _, err = policy.ApplyRetentionPolicy(ctx, w, si, true); err != nil {
			return err
		}
		if Deletable(kind) {
			return r.applyLimit(ctx, w)
		}
		return nil
	})
	return string(id), rootID, err
}

// applyLimit 删除超出份数上限的最老的快照。
func (r *Repo) applyLimit(ctx context.Context, w repo.RepositoryWriter) error {
	if r.Limit <= 0 {
		return nil
	}
	var all []*snapshot.Manifest
	for _, k := range limited {
		mans, err := snapshot.ListSnapshots(ctx, w, r.source(k))
		if err != nil {
			return err
		}
		all = append(all, mans...)
	}
	if len(all) <= r.Limit {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].StartTime.After(all[j].StartTime) })
	for _, m := range all[r.Limit:] {
		if err := w.DeleteManifest(ctx, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// Delete 删除一份定时、手动或部署前快照。占用的空间在之后的维护里回收。
func (r *Repo) Delete(ctx context.Context, id string) error {
	return repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "nagi delete snapshot"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		man, err := snapshot.LoadSnapshot(ctx, w, manifest.ID(id))
		if err != nil {
			return fmt.Errorf("读取快照: %w", err)
		}
		// 按快照来源判断种类，别的来源（版本文件）不能删。
		var kind Kind
		for _, k := range limited {
			if man.Source == r.source(k) {
				kind = k
			}
		}
		if !Deletable(kind) {
			return ErrNotDeletable
		}
		return w.DeleteManifest(ctx, man.ID)
	})
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

// FileInfo 是快照里的一个文件或目录。
type FileInfo struct {
	Name      string
	IsDir     bool
	IsSymlink bool
	Size      int64
	Modified  time.Time
	Mode      stdfs.FileMode
	UID, GID  int
}

func fileInfo(e fs.Entry) FileInfo {
	mode := e.Mode()
	owner := e.Owner()
	return FileInfo{
		Name: e.Name(), IsDir: mode.IsDir(), IsSymlink: mode&stdfs.ModeSymlink != 0, Size: e.Size(), Modified: e.ModTime(),
		Mode: mode, UID: int(owner.UserID), GID: int(owner.GroupID),
	}
}

// entry 找到快照里 name 指向的文件或目录，name 用 / 分隔。
func (r *Repo) entry(ctx context.Context, id, name string) (fs.Entry, error) {
	man, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if err != nil {
		return nil, fmt.Errorf("读取快照: %w", err)
	}
	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return nil, err
	}
	var child fs.Entry = root
	for _, part := range strings.Split(name, "/") {
		dir, ok := child.(fs.Directory)
		if !ok {
			return nil, fmt.Errorf("快照里没有 %s", name)
		}
		if child, err = dir.Child(ctx, part); err != nil {
			return nil, fmt.Errorf("快照里没有 %s: %w", name, err)
		}
	}
	return child, nil
}

// Stat 返回快照里一个文件或目录的信息。
func (r *Repo) Stat(ctx context.Context, id, name string) (FileInfo, error) {
	e, err := r.entry(ctx, id, name)
	if err != nil {
		return FileInfo{}, err
	}
	return fileInfo(e), nil
}

// ListDir 列出快照里的一个目录。
func (r *Repo) ListDir(ctx context.Context, id, name string) ([]FileInfo, error) {
	e, err := r.entry(ctx, id, name)
	if err != nil {
		return nil, err
	}
	dir, ok := e.(fs.Directory)
	if !ok {
		return nil, fmt.Errorf("%s 不是目录", name)
	}
	entries, err := fs.GetAllEntries(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, c := range entries {
		out = append(out, fileInfo(c))
	}
	return out, nil
}

// WalkFiles 遍历快照里 name 下面的所有文件和目录（包括 name 本身，sub 为空），sub 是相对 name 的路径。
// 普通文件的 open 用来读内容，其他的 open 是 nil。
func (r *Repo) WalkFiles(ctx context.Context, id, name string, fn func(sub string, f FileInfo, open func() (io.ReadCloser, error)) error) error {
	e, err := r.entry(ctx, id, name)
	if err != nil {
		return err
	}
	return walkEntry(ctx, e, "", fn)
}

func walkEntry(ctx context.Context, e fs.Entry, sub string, fn func(string, FileInfo, func() (io.ReadCloser, error)) error) error {
	var open func() (io.ReadCloser, error)
	if f, ok := e.(fs.File); ok && e.Mode().IsRegular() {
		open = func() (io.ReadCloser, error) { return f.Open(ctx) }
	}
	if err := fn(sub, fileInfo(e), open); err != nil {
		return err
	}
	dir, ok := e.(fs.Directory)
	if !ok {
		return nil
	}
	return fs.IterateEntries(ctx, dir, func(ctx context.Context, c fs.Entry) error {
		return walkEntry(ctx, c, path.Join(sub, c.Name()), fn)
	})
}

// Restore 把快照里的目录 name 恢复到 target。name 是顶层目录名，也可以用 / 指向里面的子目录
// （例如 container.web/data）。target 里多余的文件会被删除，
// 所以调用方要先决定好恢复到哪里（例如先恢复到临时目录，再按保留路径合并）。
func (r *Repo) Restore(ctx context.Context, id, name, target string) error {
	child, err := r.entry(ctx, id, name)
	if err != nil {
		return err
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

// Maintain 运行 kopia 的完整维护：回收已经没有快照引用的数据，释放磁盘空间。
// 默认使用 kopia 的安全参数，被删除的数据要过一段时间（约一两天）才真正删掉，避免影响正在进行的快照。
// immediate 只用于测试，立即回收。
func (r *Repo) Maintain(ctx context.Context, immediate bool) error {
	dr, ok := r.rep.(repo.DirectRepository)
	if !ok {
		return errors.New("快照仓库不支持维护")
	}
	safety := maintenance.SafetyFull
	if immediate {
		safety = maintenance.SafetyNone
	}
	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "nagi maintenance"}, func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
		// 这个仓库只有本节点在写，force 跳过"维护所有者"的检查（项目迁移到新节点后所有者会变）。
		return snapshotmaintenance.Run(ctx, dw, maintenance.ModeFull, true, safety)
	})
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
