package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 版本、部署记录与快照（见 docs/agent.md#版本与回滚、docs/snapshot.md）：
//   - 每次部署一个容器都生成它的一个新版本：配置（含当时的共享变量）+ 文件（data/ 中除保留路径以外的内容）+ 镜像摘要。
//   - 有状态容器部署前先保存一份它的运行数据（volumes/ 和 data/），回滚时可以选择一起恢复。
//   - 回滚 = 用旧版本的配置、文件和镜像生成这个容器的一个新版本并部署。
//   - 每次部署（一个或多个容器）在项目上记一条部署记录，可以把整个项目回到某次部署时的状态。
//   - 定时快照和手动快照保存整个项目：config/ 和每个容器的目录。

const (
	versionsKept        = 20
	deploymentsKept     = 100
	pauseLimit          = 30 * time.Second
	defaultSnapshotGap  = 60 * time.Minute
	failoverSnapshotGap = 15 * time.Minute
	metaLastSnapshot    = "last_snapshot"
	metaLastMaintain    = "last_maintenance"
	maintainGap         = 24 * time.Hour
)

func (p paths) passwordFile() string { return filepath.Join(p.configDir(), "backup.key") }

func (m *Manager) openRepo(ctx context.Context, id string, p paths) (*snapshot.Repo, error) {
	return snapshot.Open(ctx, id, p.backupsDir(), p.passwordFile(), filepath.Join(m.stateDir, id))
}

// composeFiles 返回容器调用 compose 用的文件。
func (m *Manager) composeFiles(id string, p paths, container string) compose.Files {
	mp := p.container(container)
	f := compose.Files{
		ProjectName: composeProjectName(id, container),
		ProjectDir:  mp.dataDir(),
		ComposeFile: mp.composeFile(),
		Override:    mp.overrideFile(),
		EnvFiles:    []string{mp.sharedEnvFile(), mp.envFile()},
	}
	// 项目登录过私有仓库时用它自己的 docker 配置；没有时用机器全局的（节点上全局 docker login 过的照常能用）。
	if _, err := os.Stat(filepath.Join(p.dockerConfigDir(), "config.json")); err == nil {
		f.DockerConfig = p.dockerConfigDir()
	}
	return f
}

// preservePaths 返回完整的保留路径：用户设置的 + compose 里挂载进容器的 data/ 下的目录。
func preservePaths(cfg *nagiv1.ContainerConfig, proj *types.Project, dataDir string) []string {
	return normalizePreserve(append(append([]string(nil), cfg.GetPreserve()...), bindMountPaths(proj, dataDir)...))
}

// isStateful 判断容器是否有状态：用户指定的为准；自动判断时，用了命名 volume 或有保留路径就算有状态。
func isStateful(cfg *nagiv1.ContainerConfig, proj *types.Project, preserve []string) bool {
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

// snapshotWithPause 做一份快照。有状态时暂停 label 选出的容器，超过 30 秒就放弃本次快照并恢复容器。
func (m *Manager) snapshotWithPause(ctx context.Context, rep *snapshot.Repo, label string, kind snapshot.Kind, dirs []snapshot.Dir, ignore []string, tags map[string]string, stateful bool, onLine func(string)) (string, error) {
	if !stateful {
		return rep.Create(ctx, kind, dirs, ignore, tags)
	}
	resume, err := m.docker.Pause(ctx, label)
	if err != nil {
		return "", fmt.Errorf("暂停容器: %w", err)
	}
	defer resume()
	onLine("已暂停容器，正在保存快照")
	sctx, cancel := context.WithTimeout(ctx, pauseLimit)
	defer cancel()
	sid, err := rep.Create(sctx, kind, dirs, ignore, tags)
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("暂停超过 %d 秒，已放弃本次快照并恢复容器", int(pauseLimit.Seconds()))
	}
	return sid, err
}

// ---- 部署 ----

// DeployOptions 是一次部署（一个或多个容器）的选项。
type DeployOptions struct {
	Pull       bool // 部署前从 git 拉取文件
	Upload     bool // 用面板上传的文件替换 data/（只能部署一个容器）
	PullImages bool // 部署前拉取最新的镜像
	// ApplySharedEnv 先让共享变量的草稿生效，SettingsRevision 是面板基于的项目设置修订号。
	ApplySharedEnv   bool
	SettingsRevision int64
	// Source 记在部署记录里：manual、git、image、shared_env 等，为空表示 manual。
	Source string
}

// Target 是一起部署里的一个容器。BaseRevision 不为 0 时必须和容器的修订号一致。
type Target struct {
	Container    string
	BaseRevision int64
}

// containerOpts 是部署一个容器的选项。
type containerOpts struct {
	pull         bool              // 部署前从 git 拉取文件
	upload       bool              // 用面板上传的文件替换 data/
	pullImages   bool              // 部署前拉取最新的镜像
	consumeDraft bool              // 部署成功创建版本后删除草稿
	rollbackOf   int64             // 回滚：回滚到的版本
	filesFrom    string            // 回滚：从这个版本文件快照恢复 data/
	pinImages    map[string]string // 回滚：固定使用这些镜像
	restoreData  string            // 回滚：从这个部署前快照恢复运行数据
	// sharedEnv 不为 nil 时用它代替项目当前的共享变量（回滚到版本里记的值）。
	sharedEnv []*nagiv1.EnvVar
}

// job 是一次部署里要部署的一个容器。
type job struct {
	row  projectstore.Container
	cfg  *nagiv1.ContainerConfig
	opts containerOpts
}

// Deploy 部署一个或多个容器：有草稿的容器用草稿，没有的重新部署最新版本。多个容器按依赖顺序逐个部署，
// 某个失败就停下（已经部署的保留）。整次部署记一条部署记录。只部署一个容器时返回它的新版本号。
func (m *Manager) Deploy(ctx context.Context, id string, targets []Target, o DeployOptions, onLine func(string)) (int64, error) {
	if o.Pull && o.Upload {
		return 0, invalid("不能同时从 git 拉取和使用上传的文件")
	}
	if o.Upload && len(targets) != 1 {
		return 0, invalid("上传的文件只能部署到一个容器")
	}
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	p := paths{root: row.Dir}
	if o.ApplySharedEnv {
		if err := pdb.checkRevision(ctx, o.SettingsRevision); err != nil {
			return 0, err
		}
		if err := applySharedEnv(ctx, pdb, p); err != nil {
			return 0, err
		}
		onLine("共享变量已生效")
	}
	if len(targets) == 0 {
		if o.ApplySharedEnv {
			return 0, nil
		}
		return 0, invalid("请选择要部署的容器")
	}
	var jobs []job
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.Container] {
			continue
		}
		seen[t.Container] = true
		mrow, err := pdb.container(ctx, t.Container)
		if err != nil {
			return 0, err
		}
		if err := checkContainerRevision(mrow, t.BaseRevision); err != nil {
			return 0, err
		}
		cfg, _, err := pdb.latest(ctx, t.Container)
		if err != nil {
			return 0, err
		}
		j := job{row: mrow, cfg: cfg, opts: containerOpts{pull: o.Pull, upload: o.Upload, pullImages: o.PullImages}}
		if d, err := pdb.draft(ctx, t.Container); err != nil {
			return 0, err
		} else if d != nil {
			j.cfg, j.opts.consumeDraft = d, true
		}
		if o.Upload {
			if _, err := os.Stat(p.container(t.Container).uploadFiles()); err != nil {
				return 0, invalid("容器 %s 没有已上传的文件，请先上传", t.Container)
			}
		}
		jobs = append(jobs, j)
	}
	source := o.Source
	if source == "" {
		source = "manual"
	}
	return m.runJobs(ctx, row, pdb, id, jobs, source, 0, func(j job) {
		if o.PullImages {
			// 拉取了新镜像，原来的"有新版本"提醒不再准确，下次检查时再算。
			m.clearImageUpdates(id, j.row.Name)
		}
	}, onLine)
}

// runJobs 按依赖顺序逐个部署容器，某个失败就停下，最后记一条部署记录。
func (m *Manager) runJobs(ctx context.Context, row nodestore.Project, pdb *projectDB, id string, jobs []job, source string, restoreOf int64, after func(job), onLine func(string)) (int64, error) {
	ordered, err := orderJobs(jobs)
	if err != nil {
		return 0, err
	}
	var touched []string
	var version int64
	var failErr error
	for i, j := range ordered {
		name := j.row.Name
		touched = append(touched, name)
		if len(ordered) > 1 {
			onLine(fmt.Sprintf("==== 部署容器 %s（%d/%d）====", name, i+1, len(ordered)))
		}
		v, err := m.deployContainer(ctx, row, pdb, id, j.row, j.cfg, j.opts, onLine)
		if after != nil {
			after(j)
		}
		if err != nil {
			failErr = err
			if len(ordered) > 1 {
				failErr = fmt.Errorf("容器 %s 部署失败：%w", name, err)
				var rest []string
				for _, r := range ordered[i+1:] {
					rest = append(rest, r.row.Name)
				}
				if len(rest) > 0 {
					onLine("已停止，没有部署：" + strings.Join(rest, "、"))
				}
			}
			break
		}
		version = v
	}
	m.recordDeployment(ctx, pdb, source, touched, failErr, restoreOf)
	if len(ordered) > 1 {
		version = 0
	}
	return version, failErr
}

// orderJobs 按容器设置的依赖排序（被依赖的先部署），其余按容器创建顺序。只考虑这次要部署的容器之间的依赖。
func orderJobs(jobs []job) ([]job, error) {
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].row.Position < jobs[j].row.Position })
	byName := map[string]job{}
	for _, j := range jobs {
		byName[j.row.Name] = j
	}
	var out []job
	state := map[string]int{} // 1：处理中，2：已排好
	var visit func(j job) error
	visit = func(j job) error {
		switch state[j.row.Name] {
		case 1:
			return invalid("容器之间的依赖有循环（%s）", j.row.Name)
		case 2:
			return nil
		}
		state[j.row.Name] = 1
		for _, d := range j.cfg.GetDependsOn() {
			if dep, ok := byName[d]; ok {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		state[j.row.Name] = 2
		out = append(out, j)
		return nil
	}
	for _, j := range jobs {
		if err := visit(j); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// recordDeployment 记一条部署记录：这次动到的容器，以及部署完成后所有容器各自的版本。
func (m *Manager) recordDeployment(ctx context.Context, pdb *projectDB, source string, touched []string, deployErr error, restoreOf int64) {
	rows, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return
	}
	versions := map[string]int64{}
	for _, r := range rows {
		if r.DeployedVersion > 0 {
			versions[r.Name] = r.DeployedVersion
		}
	}
	containersJSON, _ := json.Marshal(touched)
	versionsJSON, _ := json.Marshal(versions)
	msg := ""
	if deployErr != nil {
		msg = deployErr.Error()
	}
	newID, err := pdb.q.InsertDeployment(ctx, projectstore.InsertDeploymentParams{
		CreatedAt: time.Now().Unix(), Source: source, Containers: string(containersJSON), Versions: string(versionsJSON),
		Success: boolInt(deployErr == nil), Error: msg, RestoreOf: restoreOf,
	})
	if err == nil {
		_ = pdb.q.DeleteDeploymentsBefore(ctx, newID-deploymentsKept+1)
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// deployContainer 部署一个容器，生成它的一个新版本。调用方持有项目锁。
func (m *Manager) deployContainer(ctx context.Context, row nodestore.Project, pdb *projectDB, id string, mrow projectstore.Container, cfg *nagiv1.ContainerConfig, opts containerOpts, onLine func(string)) (int64, error) {
	name := mrow.Name
	p := paths{root: row.Dir}
	mp := p.container(name)
	files := m.composeFiles(id, p, name)
	settings := pdb.settings(ctx)
	shared := settings.GetSharedEnv()
	if opts.sharedEnv != nil {
		shared = opts.sharedEnv
	}
	cfg = proto.Clone(cfg).(*nagiv1.ContainerConfig)
	cp, err := validate(ctx, cfg, name, shared, mp.dataDir(), composeProjectName(id, name))
	if err != nil {
		return 0, err
	}
	preserve := preservePaths(cfg, cp, mp.dataDir())
	stateful := isStateful(cfg, cp, preserve)
	deployed := mrow.DeployedVersion

	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return 0, fmt.Errorf("打开快照仓库: %w", err)
	}
	defer rep.Close(ctx)

	// 1. 有状态容器，部署前保存一份它的运行数据，回滚时可以一起恢复。
	if stateful && deployed > 0 {
		onLine("部署前保存数据快照")
		if _, err := m.snapshotWithPause(ctx, rep, docker.ComposeLabel(files.ProjectName), snapshot.PreDeploy,
			[]snapshot.Dir{{Name: "data", Path: mp.dataDir()}, {Name: "volumes", Path: mp.volumesDir()}},
			nil, map[string]string{"container": name, "from_version": strconv.FormatInt(deployed, 10)}, true, onLine); err != nil {
			onLine("警告：部署前数据快照失败，继续部署：" + err.Error())
		}
	}

	// 2. 回滚并恢复数据：先停止这个容器，再恢复 volumes/ 和保留路径。
	if opts.restoreData != "" {
		onLine("停止容器并恢复数据")
		if err := compose.Stop(ctx, files, onLine); err != nil {
			return 0, err
		}
		if err := removeSymlinks(mp.volumesDir()); err != nil {
			return 0, err
		}
		if err := rep.Restore(ctx, opts.restoreData, "volumes", mp.volumesDir()); err != nil {
			return 0, fmt.Errorf("恢复 volumes: %w", err)
		}
		if err := restorePreserved(ctx, rep, opts.restoreData, mp, preserve); err != nil {
			return 0, err
		}
	}

	// 3. 文件：从 git 拉取、用上传的文件，或者回滚时恢复旧版本的文件。
	commit := ""
	if opts.pull {
		if commit, err = pull(ctx, mp, cfg.GetSource(), preserve, onLine); err != nil {
			return 0, err
		}
		if err := pdb.q.SetContainerCommit(ctx, projectstore.SetContainerCommitParams{LastCommit: commit, Name: name}); err != nil {
			return 0, err
		}
	}
	if opts.upload {
		onLine("用上传的文件替换 data/")
		if err := syncDir(mp.uploadFiles(), mp.dataDir(), preserve, onLine); err != nil {
			return 0, fmt.Errorf("更新文件: %w", err)
		}
		// 文件已经进了 data/，部署失败也不再保留；要重试就重新上传。
		os.RemoveAll(mp.uploadDir())
	}
	if opts.filesFrom != "" {
		onLine("恢复版本文件")
		tmp := mp.restoreDir()
		os.RemoveAll(tmp)
		defer os.RemoveAll(tmp)
		if err := rep.Restore(ctx, opts.filesFrom, "data", tmp); err != nil {
			return 0, fmt.Errorf("恢复版本文件: %w", err)
		}
		if err := syncDir(tmp, mp.dataDir(), preserve, onLine); err != nil {
			return 0, err
		}
	}

	// 4. 记录版本，保存这个版本的文件。最新版本还没成功部署过（刚创建，或者上次部署失败）时复用它的版本号。
	latest, err := pdb.q.LatestVersion(ctx, name)
	if err != nil {
		return 0, err
	}
	data, err := proto.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	sharedBlob := marshalEnv(shared)
	version := latest.Version + 1
	if !latest.DeployedAt.Valid {
		version = latest.Version
		err = pdb.q.ReplaceUndeployedVersion(ctx, projectstore.ReplaceUndeployedVersionParams{
			Config: data, SharedEnv: sharedBlob, CreatedAt: time.Now().Unix(), RollbackOf: opts.rollbackOf, Container: name, Version: version,
		})
	} else {
		err = pdb.q.InsertVersion(ctx, projectstore.InsertVersionParams{
			Container: name, Version: version, Config: data, SharedEnv: sharedBlob, CreatedAt: time.Now().Unix(), RollbackOf: opts.rollbackOf,
		})
	}
	if err != nil {
		return 0, err
	}
	if opts.consumeDraft {
		if err := pdb.q.DeleteDraft(ctx, name); err != nil {
			return version, err
		}
	}
	if err := pdb.bumpContainer(ctx, mrow); err != nil {
		return version, err
	}
	filesChanged := opts.pull || opts.upload || opts.filesFrom != ""
	if sid, root, err := rep.CreateWithRoot(ctx, snapshot.Version, []snapshot.Dir{{Name: "data", Path: mp.dataDir()}},
		preserveIgnore(preserve), map[string]string{"container": name, "version": strconv.FormatInt(version, 10)}); err != nil {
		onLine("警告：保存版本文件失败：" + err.Error())
	} else {
		_ = pdb.q.SetVersionFiles(ctx, projectstore.SetVersionFilesParams{FilesSnapshot: sid, Container: name, Version: version})
		// 文件内容和上次部署不同（例如用户用 scp 放了新文件），也要重建挂载 data/ 的服务。
		if mrow.LastFilesRoot != "" && mrow.LastFilesRoot != root {
			filesChanged = true
		}
		_ = pdb.q.SetContainerFilesRoot(ctx, projectstore.SetContainerFilesRootParams{LastFilesRoot: root, Name: name})
	}

	// 5. 配置文件、端口映射、override。
	if err := writeConfigFiles(mp, cfg, name, shared); err != nil {
		return version, err
	}
	if err := m.importPorts(ctx, pdb, p, id, mrow, cp, onLine); err != nil {
		return version, err
	}
	if err := m.writeContainerOverride(ctx, id, p, name, cfg, cp, version, settings, opts.pinImages); err != nil {
		return version, err
	}

	// 6. 启动。
	onLine(fmt.Sprintf("部署容器 %s 的版本 %d", name, version))
	if opts.pullImages {
		onLine("拉取最新的镜像")
		if err := compose.Pull(ctx, files, onLine); err != nil {
			return 0, fmt.Errorf("拉取镜像: %w", err)
		}
	}
	if err := compose.Up(ctx, files, onLine); err != nil {
		return version, err
	}
	if svcs := dataServices(cp, mp.dataDir()); filesChanged && len(svcs) > 0 {
		onLine("文件已更新，重建容器：" + strings.Join(svcs, "、"))
		if err := compose.Recreate(ctx, files, svcs, onLine); err != nil {
			return version, err
		}
	}
	if err := m.SyncNetworks(ctx, id); err != nil {
		onLine("警告：接入项目内网失败：" + err.Error())
	}
	if err := m.watchStart(ctx, files.ProjectName, deployed > 0, onLine); err != nil {
		return version, err
	}

	// 7. 记录镜像摘要和部署时间，清理过旧的版本。
	images, _ := m.docker.ProjectImages(ctx, files.ProjectName)
	imagesJSON, _ := json.Marshal(images)
	if err := pdb.q.SetVersionDeployed(ctx, projectstore.SetVersionDeployedParams{
		DeployedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		Images:     string(imagesJSON), GitCommit: commit, Container: name, Version: version,
	}); err != nil {
		return version, err
	}
	if err := pdb.q.SetContainerDeployed(ctx, projectstore.SetContainerDeployedParams{DeployedVersion: version, Name: name}); err != nil {
		return version, err
	}
	_ = pdb.q.DeleteVersionsBefore(ctx, projectstore.DeleteVersionsBeforeParams{Container: name, Version: version - versionsKept + 1})
	for _, r := range readRoutes(p, nil) {
		if r.GetContainer() != name {
			continue
		}
		if r.GetError() != "" {
			onLine("映射 " + r.GetName() + " 无效，未生效：" + r.GetError())
		} else {
			onLine("对外开放：" + routeLabel(r))
		}
	}
	return version, nil
}

// writeContainerOverride 生成并写入容器的叠加配置。
func (m *Manager) writeContainerOverride(ctx context.Context, id string, p paths, name string, cfg *nagiv1.ContainerConfig, cp *types.Project, version int64, settings *nagiv1.ProjectSettings, pins map[string]string) error {
	in := overrideInput{
		proj: cp, mp: p.container(name), projectID: id, container: name, version: version,
		single: isSingle(cfg), intranet: !settings.GetIntranetDisabled(), pins: pins,
		senv:           serviceEnv(cfg, name),
		managedRestart: m.ManagedRestart != nil && m.ManagedRestart(id),
	}
	if in.single && in.intranet {
		if err := m.ensureProjectNetwork(ctx, id); err != nil {
			return fmt.Errorf("创建项目内网: %w", err)
		}
	} else {
		subnet, err := m.subnetFor(ctx, groupOwner(id, name))
		if err != nil {
			return err
		}
		in.subnet = subnet
	}
	ov, err := buildOverride(in)
	if err != nil {
		return err
	}
	return writeOverride(in.mp.overrideFile(), ov)
}

// startDeployed 按容器当前部署的版本重新生成配置文件和叠加配置并启动，不生成新版本、镜像固定到版本记录的摘要。
// 用于恢复副本、顶替、开关内网（网段、目录或网络变了，叠加配置要重写）。
func (m *Manager) startDeployed(ctx context.Context, pdb *projectDB, p paths, id string, mrow projectstore.Container, onLine func(string)) error {
	v, err := pdb.deployed(ctx, mrow)
	if err != nil || v == nil {
		return err
	}
	cfg, err := unmarshalConfig(v.Config)
	if err != nil {
		return err
	}
	shared := unmarshalEnv(v.SharedEnv)
	mp := p.container(mrow.Name)
	cp, err := validate(ctx, proto.Clone(cfg).(*nagiv1.ContainerConfig), mrow.Name, shared, mp.dataDir(), composeProjectName(id, mrow.Name))
	if err != nil {
		return err
	}
	if err := writeConfigFiles(mp, cfg, mrow.Name, shared); err != nil {
		return err
	}
	var pins map[string]string
	_ = json.Unmarshal([]byte(v.Images), &pins)
	if err := m.writeContainerOverride(ctx, id, p, mrow.Name, cfg, cp, v.Version, pdb.settings(ctx), pins); err != nil {
		return err
	}
	if err := compose.Up(ctx, m.composeFiles(id, p, mrow.Name), onLine); err != nil {
		return err
	}
	return m.SyncNetworks(ctx, id)
}

// ---- 回滚 ----

// rollbackJob 用容器的旧版本生成一次部署：配置、文件、镜像、共享变量都用那个版本的。
func rollbackJob(ctx context.Context, pdb *projectDB, mrow projectstore.Container, target int64) (job, error) {
	v, err := pdb.q.GetVersion(ctx, projectstore.GetVersionParams{Container: mrow.Name, Version: target})
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, invalid("容器 %s 的版本 %d 不存在或已被清理", mrow.Name, target)
	} else if err != nil {
		return job{}, err
	}
	cfg, err := unmarshalConfig(v.Config)
	if err != nil {
		return job{}, err
	}
	j := job{row: mrow, cfg: cfg, opts: containerOpts{rollbackOf: target, filesFrom: v.FilesSnapshot, sharedEnv: unmarshalEnv(v.SharedEnv)}}
	if j.opts.sharedEnv == nil {
		j.opts.sharedEnv = []*nagiv1.EnvVar{}
	}
	_ = json.Unmarshal([]byte(v.Images), &j.opts.pinImages)
	return j, nil
}

// Rollback 回滚容器到某个版本：用它的配置、文件和镜像生成这个容器的一个新版本并部署。
// restoreData 时，同时把运行数据恢复到这个版本被替换之前（需要有对应的部署前快照）。
func (m *Manager) Rollback(ctx context.Context, id, container string, target int64, restoreData bool, onLine func(string)) (int64, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.container(ctx, container)
	if err != nil {
		return 0, err
	}
	j, err := rollbackJob(ctx, pdb, mrow, target)
	if err != nil {
		return 0, err
	}
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
			if s.Tags["container"] == container && s.Tags["from_version"] == strconv.FormatInt(target, 10) {
				j.opts.restoreData = s.ID
				break
			}
		}
		if j.opts.restoreData == "" {
			return 0, invalid("没有找到版本 %d 被替换前的数据快照，无法同时恢复数据", target)
		}
	}
	onLine(fmt.Sprintf("把容器 %s 回滚到版本 %d", container, target))
	return m.runJobs(ctx, row, pdb, id, []job{j}, "rollback", 0, nil, onLine)
}

// RestoreDeployment 回到某次部署时的状态：版本和那次记录不一样的容器都回滚到记录里的版本（各自生成新版本），
// 作为一次一起部署执行。之后才加的容器不动。
func (m *Manager) RestoreDeployment(ctx context.Context, id string, deploymentID int64, onLine func(string)) (int64, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	defer pdb.Close()
	d, err := pdb.q.GetDeployment(ctx, deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, invalid("部署记录 %d 不存在或已被清理", deploymentID)
	} else if err != nil {
		return 0, err
	}
	var versions map[string]int64
	_ = json.Unmarshal([]byte(d.Versions), &versions)
	containers, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return 0, err
	}
	var jobs []job
	for _, mrow := range containers {
		target, ok := versions[mrow.Name]
		if !ok {
			onLine("容器 " + mrow.Name + " 是之后才加的（或那时还没部署过），不动")
			continue
		}
		if mrow.DeployedVersion == target {
			continue
		}
		j, err := rollbackJob(ctx, pdb, mrow, target)
		if err != nil {
			return 0, err
		}
		onLine(fmt.Sprintf("容器 %s：版本 %d → 回到版本 %d", mrow.Name, mrow.DeployedVersion, target))
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return 0, invalid("所有容器的版本已经和这次部署时一样")
	}
	return m.runJobs(ctx, row, pdb, id, jobs, "restore", deploymentID, nil, onLine)
}

// restorePreserved 从快照恢复 data/ 里的保留路径（运行数据），其余文件不动。
func restorePreserved(ctx context.Context, rep *snapshot.Repo, sid string, mp containerPaths, preserve []string) error {
	if len(preserve) == 0 {
		return nil
	}
	tmp := mp.restoreDir()
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := rep.Restore(ctx, sid, "data", tmp); err != nil {
		return fmt.Errorf("恢复数据: %w", err)
	}
	// data/ 是容器可写的，保留路径的任何一级都可能被换成符号链接，所以从 data/ 开始经 os.Root 操作。
	root, err := openDirRoot(mp.dataDir())
	if err != nil {
		return err
	}
	defer root.Close()
	for _, rel := range preserve {
		src := filepath.Join(tmp, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				if err := copyFileInto(root, rel, src, info.Mode().Perm()); err != nil {
					return err
				}
			}
			continue
		}
		if err := root.MkdirAll(rel, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		sub, err := root.OpenRoot(rel)
		if err != nil {
			return err
		}
		err = syncRoot(sub, src, nil, func(string) {})
		sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- 快照 ----

// snapshotDirs 是整个项目快照包含的目录：config/ 和每个容器的目录（快照里叫 container.<容器>）。
func snapshotDirs(p paths, containers []string) []snapshot.Dir {
	dirs := []snapshot.Dir{{Name: "config", Path: p.configDir()}}
	for _, name := range containers {
		dirs = append(dirs, snapshot.Dir{Name: "container." + name, Path: p.container(name).root})
	}
	return dirs
}

var snapshotIgnore = []string{
	"/config/*.tmp", "/config/project.db-wal", "/config/project.db-shm",
	"/container.*/.checkout/", "/container.*/.restore/", "/container.*/.upload/", "/container.*/.upload-*/", "/container.*/*.tmp",
}

// projectStateful 判断项目是否有状态：只要有一个容器有状态就算。
func (m *Manager) projectStateful(ctx context.Context, pdb *projectDB, id string, p paths) bool {
	containers, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return false
	}
	shared := pdb.settings(ctx).GetSharedEnv()
	for _, r := range containers {
		cfg, err := pdb.current(ctx, r)
		if err != nil {
			continue
		}
		mp := p.container(r.Name)
		if cp, err := loadContainer(ctx, cfg, r.Name, shared, mp.dataDir(), composeProjectName(id, r.Name)); err == nil &&
			isStateful(cfg, cp, preservePaths(cfg, cp, mp.dataDir())) {
			return true
		}
	}
	return false
}

func containerNames(ctx context.Context, pdb *projectDB) ([]string, error) {
	rows, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}

// TakeSnapshot 做一份定时或手动快照：config/ 和所有容器的目录。有状态项目快照时暂停所有容器的容器。
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
	containers, err := containerNames(ctx, pdb)
	if err != nil {
		return "", err
	}
	stateful := m.projectStateful(ctx, pdb, id, p)
	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)
	sid, err := m.snapshotWithPause(ctx, rep, projectLabel(id), kind, snapshotDirs(p, containers), snapshotIgnore, nil, stateful, onLine)
	if err != nil {
		return "", err
	}
	_ = pdb.setMeta(ctx, metaLastSnapshot, strconv.FormatInt(time.Now().Unix(), 10))
	onLine("快照已保存")
	return sid, nil
}

// SnapshotDue 给到了间隔时间、部署过的项目做定时快照。由节点每分钟调用一次。
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
		deployed := false
		if containers, err := pdb.q.ListContainers(ctx); err == nil {
			for _, mem := range containers {
				deployed = deployed || mem.DeployedVersion > 0
			}
		}
		last, _ := strconv.ParseInt(pdb.meta(ctx, metaLastSnapshot), 10, 64)
		interval := pdb.settings(ctx).GetSnapshotIntervalMinutes()
		pdb.Close()
		if !deployed {
			continue
		}
		gap := defaultSnapshotGap
		if interval > 0 {
			gap = time.Duration(interval) * time.Minute
		}
		// 设置了顶替的项目每 15 分钟一次（与同步给热备的频率一致），除非用户设得更短。
		if m.FastSnapshot != nil && m.FastSnapshot(r.ID) && gap > failoverSnapshotGap {
			gap = failoverSnapshotGap
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

// RestoreSnapshot 从定时或手动快照恢复 data/ 和 volumes/：container 为空时恢复整个项目（所有部署过的容器），
// 否则只恢复这个容器。先停止、恢复，再重新启动。配置不变。
func (m *Manager) RestoreSnapshot(ctx context.Context, id, sid, container string, onLine func(string)) error {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	defer pdb.Close()
	p := paths{root: row.Dir}
	containers, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return err
	}
	var targets []string
	for _, r := range containers {
		if (container == "" || r.Name == container) && r.DeployedVersion > 0 {
			targets = append(targets, r.Name)
		}
	}
	if container != "" && len(targets) == 0 {
		return invalid("容器 %s 不存在或还没有部署过", container)
	}
	if len(targets) == 0 {
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
	for _, name := range targets {
		onLine("停止容器 " + name)
		if err := compose.Stop(ctx, m.composeFiles(id, p, name), onLine); err != nil {
			return err
		}
	}
	for _, name := range targets {
		mp := p.container(name)
		onLine("恢复容器 " + name + " 的 data/ 和 volumes/")
		if err := restoreContainerDir(ctx, rep, sid, name, "data", mp.dataDir()); err != nil {
			onLine("快照里没有容器 " + name + " 的数据，跳过：" + err.Error())
			continue
		}
		if err := restoreContainerDir(ctx, rep, sid, name, "volumes", mp.volumesDir()); err != nil {
			return fmt.Errorf("恢复 volumes: %w", err)
		}
	}
	for _, name := range targets {
		onLine("重新启动容器 " + name)
		if err := compose.Up(ctx, m.composeFiles(id, p, name), onLine); err != nil {
			return err
		}
	}
	_ = m.SyncNetworks(ctx, id)
	onLine("已从快照恢复")
	return nil
}

// ---- 列表 ----

// Versions 列出容器的版本，最新的在前。
func (m *Manager) Versions(ctx context.Context, id, container string) ([]*nagiv1.Version, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	if _, err := pdb.container(ctx, container); err != nil {
		return nil, err
	}
	rows, err := pdb.q.ListVersions(ctx, container)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.Version, 0, len(rows))
	for _, v := range rows {
		ver := &nagiv1.Version{
			Container:  container,
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

// Deployments 列出项目的部署记录，最新的在前。
func (m *Manager) Deployments(ctx context.Context, id string) ([]*nagiv1.Deployment, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	rows, err := pdb.q.ListDeployments(ctx, deploymentsKept)
	if err != nil {
		return nil, err
	}
	out := make([]*nagiv1.Deployment, 0, len(rows))
	for _, d := range rows {
		dep := &nagiv1.Deployment{
			Id: d.ID, Time: timestamppb.New(time.Unix(d.CreatedAt, 0)), Source: d.Source,
			Success: d.Success == 1, Error: d.Error, RestoreOf: d.RestoreOf,
		}
		_ = json.Unmarshal([]byte(d.Containers), &dep.Containers)
		_ = json.Unmarshal([]byte(d.Versions), &dep.Versions)
		out = append(out, dep)
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
		snap := &nagiv1.Snapshot{Id: s.ID, Kind: string(s.Kind), Time: timestamppb.New(s.Time), Size: s.Size, Container: s.Tags["container"]}
		v := s.Tags["version"]
		if s.Kind == snapshot.PreDeploy {
			v = s.Tags["from_version"]
		}
		snap.Version, _ = strconv.ParseInt(v, 10, 64)
		out = append(out, snap)
	}
	return out
}

// 快照里每个容器的目录叫 container.<名字>，版本文件和部署前快照打的标签是 container=<名字>。

// restoreContainerDir 从快照里恢复一个容器的 data/ 或 volumes/（sub）到 dst。
// 恢复是原地覆盖的，先删掉容器可能留下的符号链接（见 removeSymlinks），调用前容器要已经停止。
func restoreContainerDir(ctx context.Context, rep *snapshot.Repo, sid, name, sub, dst string) error {
	if err := removeSymlinks(dst); err != nil {
		return err
	}
	return rep.Restore(ctx, sid, "container."+name+"/"+sub, dst)
}
