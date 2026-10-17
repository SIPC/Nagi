package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 版本与快照：
//   - 每次部署都生成一个新版本：配置 + 文件（data/ 中除保留路径以外的内容）+ 镜像摘要。
//   - 有状态项目部署前先保存一份运行数据（volumes/ 和 data/），回滚时可以选择一起恢复。
//   - 回滚 = 用旧版本的配置、文件和镜像生成一个新版本并部署。
//   - 定时快照和手动快照保存 data/、volumes/、config/。

const (
	versionsKept       = 20
	pauseLimit         = 30 * time.Second
	defaultSnapshotGap = 60 * time.Minute
	metaLastFilesRoot  = "last_files_root"
	metaLastSnapshot   = "last_snapshot"
	metaLastMaintain   = "last_maintenance"
	maintainGap        = 24 * time.Hour
)

func (p paths) passwordFile() string { return filepath.Join(p.configDir(), "backup.key") }
func (p paths) restoreDir() string   { return filepath.Join(p.configDir(), ".restore") }

func (m *Manager) openRepo(ctx context.Context, id string, p paths) (*snapshot.Repo, error) {
	return snapshot.Open(ctx, id, p.backupsDir(), p.passwordFile(), filepath.Join(m.stateDir, id))
}

func composeFiles(id string, p paths) compose.Files {
	return compose.Files{
		ProjectName: composeProjectName(id),
		ProjectDir:  p.dataDir(),
		ComposeFile: p.composeFile(),
		Override:    p.overrideFile(),
		EnvFile:     p.envFile(),
	}
}

// preservePaths 返回完整的保留路径：用户设置的 + compose 里挂载进容器的 data/ 下的目录。
func preservePaths(cfg *nagiv1.ProjectConfig, proj *types.Project, dataDir string) []string {
	return normalizePreserve(append(append([]string(nil), cfg.GetPreserve()...), bindMountPaths(proj, dataDir)...))
}

// isStateful 判断项目是否有状态：用户指定的为准；自动判断时，用了命名 volume 或有保留路径就算有状态。
func isStateful(cfg *nagiv1.ProjectConfig, proj *types.Project, preserve []string) bool {
	switch cfg.GetStateful() {
	case nagiv1.Stateful_STATEFUL_YES:
		return true
	case nagiv1.Stateful_STATEFUL_NO:
		return false
	}
	if len(preserve) > 0 {
		return true
	}
	for _, v := range proj.Volumes {
		if !v.External {
			return true
		}
	}
	return false
}

func preserveIgnore(preserve []string) []string {
	var out []string
	for _, p := range preserve {
		out = append(out, "/data/"+p)
	}
	return out
}

// snapshotWithPause 做一份快照。有状态项目快照时暂停容器，超过 30 秒就放弃本次快照并恢复容器。
func (m *Manager) snapshotWithPause(ctx context.Context, rep *snapshot.Repo, id string, kind snapshot.Kind, dirs []snapshot.Dir, ignore []string, tags map[string]string, stateful bool, onLine func(string)) (string, error) {
	if !stateful {
		return rep.Create(ctx, kind, dirs, ignore, tags)
	}
	resume, err := m.docker.PauseProject(ctx, composeProjectName(id))
	if err != nil {
		return "", fmt.Errorf("暂停容器: %w", err)
	}
	defer resume()
	onLine("已暂停容器，正在保存快照")
	sctx, cancel := context.WithTimeout(ctx, pauseLimit)
	defer cancel()
	sid, err := rep.Create(sctx, kind, dirs, ignore, tags)
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("暂停超过 %d 秒，已放弃本次快照并恢复容器；数据较多时可以考虑改用导出方式", int(pauseLimit.Seconds()))
	}
	return sid, err
}

// ---- 部署 ----

// deployOpts 是一次部署的选项。
type deployOpts struct {
	pull         bool              // 部署前从 git 拉取文件
	upload       bool              // 用面板上传的文件替换 data/
	consumeDraft bool              // 部署成功创建版本后删除草稿
	rollbackOf   int64             // 回滚：回滚到的版本
	filesFrom    string            // 回滚：从这个版本文件快照恢复 data/
	pinImages    map[string]string // 回滚：固定使用这些镜像
	restoreData  string            // 回滚：从这个部署前快照恢复运行数据
}

// Deploy 部署项目，生成一个新版本。有草稿时用草稿，没有时用最新版本的配置（重新部署）。
// baseRevision 不为 0 时，必须和节点上的修订号一致。
// doPull 时先从 git 拉取文件；useUpload 时用面板上传的文件替换 data/。
func (m *Manager) Deploy(ctx context.Context, id string, baseRevision int64, doPull, useUpload bool, onLine func(string)) (int64, error) {
	if doPull && useUpload {
		return 0, invalid("不能同时从 git 拉取和使用上传的文件")
	}
	row, pdb, revision, unlock, err := m.projectForUpdate(ctx, id, baseRevision)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	cfg, _, err := pdb.latest(ctx)
	if err != nil {
		return 0, err
	}
	opts := deployOpts{pull: doPull, upload: useUpload}
	if useUpload {
		if _, err := os.Stat(paths{root: row.Dir}.uploadFiles()); err != nil {
			return 0, invalid("没有已上传的文件，请先上传")
		}
	}
	if draft, err := pdb.draft(ctx); err != nil {
		return 0, err
	} else if draft != nil {
		cfg, opts.consumeDraft = draft, true
	}
	return m.deploy(ctx, row, pdb, revision, cfg, opts, onLine)
}

// Rollback 回滚到某个版本：用它的配置、文件和镜像生成一个新版本并部署。
// restoreData 时，同时把运行数据恢复到这个版本被替换之前（需要有对应的部署前快照）。
func (m *Manager) Rollback(ctx context.Context, id string, target int64, restoreData bool, onLine func(string)) (int64, error) {
	row, pdb, revision, unlock, err := m.projectForUpdate(ctx, id, 0)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	v, err := pdb.q.GetVersion(ctx, target)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, invalid("版本 %d 不存在或已被清理", target)
	} else if err != nil {
		return 0, err
	}
	cfg := &nagiv1.ProjectConfig{}
	if err := proto.Unmarshal(v.Config, cfg); err != nil {
		return 0, err
	}
	opts := deployOpts{rollbackOf: target, filesFrom: v.FilesSnapshot}
	_ = json.Unmarshal([]byte(v.Images), &opts.pinImages)
	if restoreData {
		rep, err := m.openRepo(ctx, id, paths{root: row.Dir})
		if err != nil {
			return 0, err
		}
		list, err := rep.List(ctx, snapshot.PreDeploy)
		rep.Close(ctx)
		if err != nil {
			return 0, err
		}
		for _, s := range list { // 最新的在前
			if s.Tags["from_version"] == strconv.FormatInt(target, 10) {
				opts.restoreData = s.ID
				break
			}
		}
		if opts.restoreData == "" {
			return 0, invalid("没有找到版本 %d 被替换前的数据快照，无法同时恢复数据", target)
		}
	}
	onLine(fmt.Sprintf("回滚到版本 %d", target))
	return m.deploy(ctx, row, pdb, revision, cfg, opts, onLine)
}

func (m *Manager) deploy(ctx context.Context, row nodestore.Project, pdb *projectDB, revision int64, cfg *nagiv1.ProjectConfig, opts deployOpts, onLine func(string)) (int64, error) {
	id, p := row.ID, paths{root: row.Dir}
	files := composeFiles(id, p)

	proj, err := validate(ctx, cfg, p.dataDir(), composeProjectName(id))
	if err != nil {
		return 0, err
	}
	preserve := preservePaths(cfg, proj, p.dataDir())
	stateful := isStateful(cfg, proj, preserve)
	deployed, _ := strconv.ParseInt(pdb.meta(ctx, metaDeployedVersion), 10, 64)

	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return 0, fmt.Errorf("打开快照仓库: %w", err)
	}
	defer rep.Close(ctx)

	// 1. 有状态项目，部署前保存一份运行数据，回滚时可以一起恢复。
	if stateful && deployed > 0 {
		onLine("部署前保存数据快照")
		if _, err := m.snapshotWithPause(ctx, rep, id, snapshot.PreDeploy,
			[]snapshot.Dir{{Name: "data", Path: p.dataDir()}, {Name: "volumes", Path: p.volumesDir()}},
			nil, map[string]string{"from_version": strconv.FormatInt(deployed, 10)}, true, onLine); err != nil {
			onLine("警告：部署前数据快照失败，继续部署：" + err.Error())
		}
	}

	// 2. 回滚并恢复数据：先停止服务，再恢复 volumes/ 和保留路径。
	if opts.restoreData != "" {
		onLine("停止服务并恢复数据")
		if err := compose.Stop(ctx, files, onLine); err != nil {
			return 0, err
		}
		if err := rep.Restore(ctx, opts.restoreData, "volumes", p.volumesDir()); err != nil {
			return 0, fmt.Errorf("恢复 volumes: %w", err)
		}
		if err := m.restorePreserved(ctx, rep, opts.restoreData, p, preserve); err != nil {
			return 0, err
		}
	}

	// 3. 文件：从 git 拉取，或者回滚时恢复旧版本的文件。
	commit := ""
	if opts.pull {
		if commit, err = pull(ctx, p, cfg.GetSource(), preserve, onLine); err != nil {
			return 0, err
		}
		if err := pdb.setMeta(ctx, metaLastCommit, commit); err != nil {
			return 0, err
		}
	}
	if opts.upload {
		onLine("用上传的文件替换 data/")
		if err := syncDir(p.uploadFiles(), p.dataDir(), preserve, onLine); err != nil {
			return 0, fmt.Errorf("更新文件: %w", err)
		}
		// 文件已经进了 data/，部署失败也不再保留；要重试就重新上传。
		os.RemoveAll(p.uploadDir())
	}
	if opts.filesFrom != "" {
		onLine("恢复版本文件")
		tmp := p.restoreDir()
		os.RemoveAll(tmp)
		defer os.RemoveAll(tmp)
		if err := rep.Restore(ctx, opts.filesFrom, "data", tmp); err != nil {
			return 0, fmt.Errorf("恢复版本文件: %w", err)
		}
		if err := syncDir(tmp, p.dataDir(), preserve, onLine); err != nil {
			return 0, err
		}
	}

	// 4. 记录版本，保存这个版本的文件。最新版本还没成功部署过（刚创建，或者上次部署失败）时复用它的版本号。
	latest, err := pdb.q.LatestVersion(ctx)
	if err != nil {
		return 0, err
	}
	data, err := proto.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	version := latest.Version + 1
	if !latest.DeployedAt.Valid {
		version = latest.Version
		err = pdb.q.ReplaceUndeployedVersion(ctx, projectstore.ReplaceUndeployedVersionParams{
			Config: data, CreatedAt: time.Now().Unix(), RollbackOf: opts.rollbackOf, Version: version,
		})
	} else {
		err = pdb.q.InsertRollbackVersion(ctx, projectstore.InsertRollbackVersionParams{
			Version: version, Config: data, CreatedAt: time.Now().Unix(), RollbackOf: opts.rollbackOf,
		})
	}
	if err != nil {
		return 0, err
	}
	if opts.consumeDraft {
		if err := pdb.q.DeleteDraft(ctx); err != nil {
			return version, err
		}
	}
	if err := pdb.setRevision(ctx, revision+1); err != nil {
		return version, err
	}
	filesChanged := opts.pull || opts.filesFrom != ""
	if sid, root, err := rep.CreateWithRoot(ctx, snapshot.Version, []snapshot.Dir{{Name: "data", Path: p.dataDir()}},
		preserveIgnore(preserve), map[string]string{"version": strconv.FormatInt(version, 10)}); err != nil {
		onLine("警告：保存版本文件失败：" + err.Error())
	} else {
		_ = pdb.q.SetVersionFiles(ctx, projectstore.SetVersionFilesParams{FilesSnapshot: sid, Version: version})
		// 文件内容和上次部署不同（例如用户用 scp 放了新文件），也要重建挂载 data/ 的服务。
		if last := pdb.meta(ctx, metaLastFilesRoot); last != "" && last != root {
			filesChanged = true
		}
		_ = pdb.setMeta(ctx, metaLastFilesRoot, root)
	}

	// 5. 配置文件、端口映射、override。
	if err := writeConfigFiles(p, cfg); err != nil {
		return version, err
	}
	if err := m.importPorts(ctx, pdb, p, id, proj, onLine); err != nil {
		return version, err
	}
	override, err := buildOverride(proj, p, id, version, row.Subnet, serviceEnv(cfg), opts.pinImages)
	if err != nil {
		return version, err
	}
	if err := writeOverride(p.overrideFile(), override); err != nil {
		return version, err
	}

	// 6. 启动。
	onLine(fmt.Sprintf("部署版本 %d", version))
	if err := compose.Up(ctx, files, onLine); err != nil {
		return version, err
	}
	if svcs := dataServices(proj, p.dataDir()); filesChanged && len(svcs) > 0 {
		onLine("文件已更新，重建服务：" + strings.Join(svcs, "、"))
		if err := compose.Recreate(ctx, files, svcs, onLine); err != nil {
			return version, err
		}
	}

	// 7. 记录镜像摘要和部署时间，清理过旧的版本。
	images, _ := m.docker.ProjectImages(ctx, composeProjectName(id))
	imagesJSON, _ := json.Marshal(images)
	if err := pdb.q.SetVersionDeployed(ctx, projectstore.SetVersionDeployedParams{
		DeployedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		Images:     string(imagesJSON), GitCommit: commit, Version: version,
	}); err != nil {
		return version, err
	}
	if err := pdb.setMeta(ctx, metaDeployedVersion, strconv.FormatInt(version, 10)); err != nil {
		return version, err
	}
	_ = pdb.q.DeleteVersionsBefore(ctx, version-versionsKept+1)
	for _, r := range readRoutes(p) {
		if r.GetError() != "" {
			onLine("映射 " + r.GetName() + " 无效，未生效：" + r.GetError())
		} else {
			onLine("对外开放：" + routeLabel(r))
		}
	}
	return version, nil
}

// restorePreserved 从快照恢复 data/ 里的保留路径（运行数据），其余文件不动。
func (m *Manager) restorePreserved(ctx context.Context, rep *snapshot.Repo, sid string, p paths, preserve []string) error {
	if len(preserve) == 0 {
		return nil
	}
	tmp := p.restoreDir()
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := rep.Restore(ctx, sid, "data", tmp); err != nil {
		return fmt.Errorf("恢复数据: %w", err)
	}
	for _, rel := range preserve {
		src := filepath.Join(tmp, filepath.FromSlash(rel))
		dst := filepath.Join(p.dataDir(), filepath.FromSlash(rel))
		info, err := os.Stat(src)
		if errors.Is(err, os.ErrNotExist) {
			os.RemoveAll(dst)
			continue
		} else if err != nil {
			return err
		}
		if info.IsDir() {
			if err := syncDir(src, dst, nil, func(string) {}); err != nil {
				return err
			}
		} else {
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 快照 ----

// TakeSnapshot 做一份定时或手动快照：data/、volumes/、config/。
func (m *Manager) TakeSnapshot(ctx context.Context, id string, kind snapshot.Kind, onLine func(string)) (string, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return "", err
	}
	p := paths{root: row.Dir}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return "", err
	}
	defer pdb.Close()
	cfg, _, err := pdb.latest(ctx)
	if err != nil {
		return "", err
	}
	stateful := false
	if proj, err := validate(ctx, proto.Clone(cfg).(*nagiv1.ProjectConfig), p.dataDir(), composeProjectName(id)); err == nil {
		stateful = isStateful(cfg, proj, preservePaths(cfg, proj, p.dataDir()))
	}
	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)
	sid, err := m.snapshotWithPause(ctx, rep, id, kind, snapshotDirs(p), snapshotIgnore, nil, stateful, onLine)
	if err != nil {
		return "", err
	}
	_ = pdb.setMeta(ctx, metaLastSnapshot, strconv.FormatInt(time.Now().Unix(), 10))
	onLine("快照已保存")
	return sid, nil
}

// SnapshotDue 给到了间隔时间的已部署项目做定时快照。由节点每分钟调用一次。
func (m *Manager) SnapshotDue(ctx context.Context, onResult func(id string, err error)) {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		pdb, err := openProject(ctx, r.Dir)
		if err != nil {
			continue
		}
		deployed := pdb.meta(ctx, metaDeployedVersion)
		last, _ := strconv.ParseInt(pdb.meta(ctx, metaLastSnapshot), 10, 64)
		cfg, _, _ := pdb.latest(ctx)
		pdb.Close()
		if deployed == "" || deployed == "0" {
			continue
		}
		gap := defaultSnapshotGap
		if n := cfg.GetSnapshotIntervalMinutes(); n > 0 {
			gap = time.Duration(n) * time.Minute
		}
		if time.Since(time.Unix(last, 0)) < gap {
			continue
		}
		_, err = m.TakeSnapshot(ctx, r.ID, snapshot.Scheduled, func(string) {})
		if err == nil {
			if err = m.maintainIfDue(ctx, r.ID); err != nil {
				err = fmt.Errorf("快照仓库维护: %w", err)
			}
		}
		if onResult != nil {
			onResult(r.ID, err)
		}
	}
}

// maintainIfDue 每个项目每天运行一次快照仓库维护，回收过期快照占用的空间。
func (m *Manager) maintainIfDue(ctx context.Context, id string) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	last, _ := strconv.ParseInt(pdb.meta(ctx, metaLastMaintain), 10, 64)
	if time.Since(time.Unix(last, 0)) < maintainGap {
		return nil
	}
	rep, err := m.openRepo(ctx, id, paths{root: row.Dir})
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	if err := rep.Maintain(ctx, false); err != nil {
		return err
	}
	return pdb.setMeta(ctx, metaLastMaintain, strconv.FormatInt(time.Now().Unix(), 10))
}

// RestoreSnapshot 从定时或手动快照恢复 data/ 和 volumes/：停止服务、恢复、重新启动。配置不变。
func (m *Manager) RestoreSnapshot(ctx context.Context, id, sid string, onLine func(string)) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	p := paths{root: row.Dir}
	if _, err := os.Stat(p.overrideFile()); err != nil {
		return invalid("项目还没有部署过")
	}
	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	tags, err := rep.Tags(ctx, sid)
	if err != nil {
		return invalid("快照不存在：%v", err)
	}
	if k := snapshot.Kind(tags["kind"]); k != snapshot.Scheduled && k != snapshot.Manual {
		return invalid("只能从定时快照或手动快照恢复")
	}
	files := composeFiles(id, p)
	onLine("停止服务")
	if err := compose.Stop(ctx, files, onLine); err != nil {
		return err
	}
	onLine("恢复 data/ 和 volumes/")
	if err := rep.Restore(ctx, sid, "data", p.dataDir()); err != nil {
		return fmt.Errorf("恢复 data: %w", err)
	}
	if err := rep.Restore(ctx, sid, "volumes", p.volumesDir()); err != nil {
		return fmt.Errorf("恢复 volumes: %w", err)
	}
	onLine("重新启动服务")
	if err := compose.Up(ctx, files, onLine); err != nil {
		return err
	}
	onLine("已从快照恢复")
	return nil
}

// ---- 列表 ----

// Versions 列出项目的版本，最新的在前。
func (m *Manager) Versions(ctx context.Context, id string) ([]*nagiv1.Version, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	rows, err := pdb.q.ListVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.Version, 0, len(rows))
	for _, v := range rows {
		ver := &nagiv1.Version{
			Version:    v.Version,
			CreatedAt:  timestamppb.New(time.Unix(v.CreatedAt, 0)),
			HasFiles:   v.FilesSnapshot != "",
			RollbackOf: v.RollbackOf,
			Commit:     v.GitCommit,
		}
		if v.DeployedAt.Valid {
			ver.DeployedAt = timestamppb.New(time.Unix(v.DeployedAt.Int64, 0))
		}
		_ = json.Unmarshal([]byte(v.Images), &ver.Images)
		out = append(out, ver)
	}
	return out, nil
}

// Snapshots 列出项目的快照，最新的在前。
func (m *Manager) Snapshots(ctx context.Context, id string) ([]*nagiv1.Snapshot, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	rep, err := m.openRepo(ctx, id, paths{root: row.Dir})
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)
	list, err := rep.List(ctx)
	if err != nil {
		return nil, err
	}
	return snapshotsProto(list), nil
}

func snapshotsProto(list []snapshot.Info) []*nagiv1.Snapshot {
	out := make([]*nagiv1.Snapshot, 0, len(list))
	for _, s := range list {
		snap := &nagiv1.Snapshot{Id: s.ID, Kind: string(s.Kind), Time: timestamppb.New(s.Time), Size: s.Size}
		v := s.Tags["version"]
		if s.Kind == snapshot.PreDeploy {
			v = s.Tags["from_version"]
		}
		snap.Version, _ = strconv.ParseInt(v, 10, 64)
		out = append(out, snap)
	}
	return out
}
