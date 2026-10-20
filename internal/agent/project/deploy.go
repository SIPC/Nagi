package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/protobuf/proto"
)

// 部署（见 docs/agent.md#修改与部署）：面板定好新版本的配置，连同共享变量、镜像仓库认证、deploy key 一起下发；
// 节点写好容器目录里的文件、执行 compose、观察 10 秒，把镜像摘要报回面板，成功后缓存这份配置。

// DeployOptions 是部署一个容器的选项。
type DeployOptions struct {
	Pull       bool // 部署前从 git 拉取文件
	Upload     bool // 用面板上传的文件替换 data/
	PullImages bool // 部署前拉取最新的镜像
	// FilesFrom：回滚时用这个版本保存的文件替换 data/。
	FilesFrom int64
	// RestoreDataFrom：回滚并恢复数据时，把运行数据恢复到这个版本被替换之前。
	RestoreDataFrom int64
	// Previous 是容器之前部署过的版本，0 表示第一次部署。
	Previous int64
}

// DeployResult 是部署的结果。
type DeployResult struct {
	Images   map[string]string
	Commit   string
	HasFiles bool
}

// Deploy 部署一个容器：s.Version 是这次部署的版本号。容器还没放到本节点时先检查并建好目录。
func (m *Manager) Deploy(ctx context.Context, s *nagiv1.ContainerSpec, o DeployOptions, onLine func(string)) (DeployResult, error) {
	var res DeployResult
	if err := checkSpec(s); err != nil {
		return res, err
	}
	if s.GetVersion() <= 0 {
		return res, invalid("缺少版本号")
	}
	if o.Pull && o.Upload {
		return res, invalid("不能同时从 git 拉取和使用上传的文件")
	}
	l := m.lock(s.GetProjectId(), s.GetName())
	l.Lock()
	defer l.Unlock()

	s = proto.Clone(s).(*nagiv1.ContainerSpec)
	cached, err := m.Get(ctx, s.GetProjectId(), s.GetName())
	filesRoot := ""
	switch {
	case err == nil:
		if cached.Spec.GetDir() != s.GetDir() {
			return res, invalid("容器目录不能修改（%s）", cached.Spec.GetDir())
		}
		filesRoot = cached.FilesRoot
	case errors.Is(err, ErrNotFound):
		dir, err := m.CheckDir(ctx, s.GetDir())
		if err != nil {
			return res, err
		}
		s.Dir = dir
	default:
		return res, err
	}
	mp := containerPaths{root: s.GetDir()}
	if err := m.prepareDir(mp); err != nil {
		return res, err
	}
	if cached == nil {
		// 先缓存起来（版本号还是 0：还没部署成功），部署失败时这个容器也在本节点上。
		first := proto.Clone(s).(*nagiv1.ContainerSpec)
		first.Version, first.Images = 0, nil
		if err := m.store(ctx, first); err != nil {
			return res, err
		}
	}

	name := s.GetName()
	files := composeFiles(s)
	cp, err := spec.Validate(ctx, proto.Clone(s.GetConfig()).(*nagiv1.ContainerConfig), name, s.GetSharedEnv(), mp.dataDir(), files.ProjectName)
	if err != nil {
		return res, err
	}
	preserve := spec.PreservePaths(s.GetConfig(), cp, mp.dataDir())
	stateful := spec.IsStateful(s.GetConfig(), cp, preserve)

	if err := writeSecrets(mp, s); err != nil {
		return res, err
	}
	rep, err := m.openRepo(ctx, s)
	if err != nil {
		return res, fmt.Errorf("打开快照仓库: %w", err)
	}
	defer rep.Close(ctx)

	// 1. 有状态容器，部署前保存一份它的运行数据，回滚时可以一起恢复。
	if stateful && o.Previous > 0 {
		onLine("部署前保存数据快照")
		if _, err := m.snapshotWithPause(ctx, rep, containerLabel(s.GetProjectId(), name), snapshot.PreDeploy,
			[]snapshot.Dir{{Name: "data", Path: mp.dataDir()}, {Name: "volumes", Path: mp.volumesDir()}},
			nil, map[string]string{"container": name, "from_version": strconv.FormatInt(o.Previous, 10)}, true, onLine); err != nil {
			onLine("警告：部署前数据快照失败，继续部署：" + err.Error())
		}
	}

	// 2. 回滚并恢复数据：先停止这个容器，再恢复 volumes/ 和保留路径。
	if o.RestoreDataFrom > 0 {
		sid, err := findPreDeploy(ctx, rep, o.RestoreDataFrom)
		if err != nil {
			return res, err
		}
		onLine("停止容器并恢复数据")
		if _, err := os.Stat(mp.overrideFile()); err == nil {
			if err := compose.Stop(ctx, files, onLine); err != nil {
				return res, err
			}
		}
		if err := removeSymlinks(mp.volumesDir()); err != nil {
			return res, err
		}
		if err := rep.Restore(ctx, sid, "volumes", mp.volumesDir()); err != nil {
			return res, fmt.Errorf("恢复 volumes: %w", err)
		}
		if err := restorePreserved(ctx, rep, sid, mp, preserve); err != nil {
			return res, err
		}
	}

	// 3. 文件：从 git 拉取、用上传的文件，或者回滚时恢复旧版本的文件。
	if o.Pull {
		if res.Commit, err = pull(ctx, mp, s.GetConfig().GetSource(), preserve, onLine); err != nil {
			return res, err
		}
	}
	if o.Upload {
		if _, err := os.Stat(mp.uploadFiles()); err != nil {
			return res, invalid("容器 %s 没有已上传的文件，请先上传", name)
		}
		onLine("用上传的文件替换 data/")
		if err := syncDir(mp.uploadFiles(), mp.dataDir(), preserve, onLine); err != nil {
			return res, fmt.Errorf("更新文件: %w", err)
		}
		// 文件已经进了 data/，部署失败也不再保留；要重试就重新上传。
		os.RemoveAll(mp.uploadDir())
	}
	if o.FilesFrom > 0 {
		sid, err := findVersionFiles(ctx, rep, o.FilesFrom)
		if err != nil {
			return res, err
		}
		onLine("恢复版本文件")
		tmp := mp.restoreDir()
		os.RemoveAll(tmp)
		defer os.RemoveAll(tmp)
		if err := rep.Restore(ctx, sid, "data", tmp); err != nil {
			return res, fmt.Errorf("恢复版本文件: %w", err)
		}
		if err := syncDir(tmp, mp.dataDir(), preserve, onLine); err != nil {
			return res, err
		}
	}

	// 4. 保存这个版本的文件，用来回滚。
	filesChanged := o.Pull || o.Upload || o.FilesFrom > 0
	if _, root, err := rep.CreateWithRoot(ctx, snapshot.Version, []snapshot.Dir{{Name: "data", Path: mp.dataDir()}},
		preserveIgnore(preserve), map[string]string{"container": name, "version": strconv.FormatInt(s.GetVersion(), 10)}); err != nil {
		onLine("警告：保存版本文件失败：" + err.Error())
	} else {
		res.HasFiles = true
		// 文件内容和上次部署不同（例如用户用 scp 放了新文件），也要重建挂载 data/ 的服务。
		if filesRoot != "" && filesRoot != root {
			filesChanged = true
		}
		filesRoot = root
	}

	// 5. 配置文件、叠加配置。
	if err := writeConfigFiles(mp, s.GetConfig(), name, s.GetSharedEnv()); err != nil {
		return res, err
	}
	if err := m.writeOverride(ctx, s, cp); err != nil {
		return res, err
	}

	// 6. 启动。
	onLine(fmt.Sprintf("部署容器 %s 的版本 %d", name, s.GetVersion()))
	if o.PullImages {
		onLine("拉取最新的镜像")
		if err := compose.Pull(ctx, files, onLine); err != nil {
			return res, fmt.Errorf("拉取镜像: %w", err)
		}
		m.clearImageUpdates(s.GetProjectId(), name)
	}
	if err := compose.Up(ctx, files, onLine); err != nil {
		return res, err
	}
	if svcs := spec.DataServices(cp, mp.dataDir()); filesChanged && len(svcs) > 0 {
		onLine("文件已更新，重建容器：" + strings.Join(svcs, "、"))
		if err := compose.Recreate(ctx, files, svcs, onLine); err != nil {
			return res, err
		}
	}
	if err := m.SyncNetworks(ctx, s.GetProjectId()); err != nil {
		onLine("警告：接入项目内网失败：" + err.Error())
	}
	if err := m.watchStart(ctx, files.ProjectName, o.Previous > 0, onLine); err != nil {
		return res, err
	}

	// 7. 记下镜像摘要，缓存这份配置。
	res.Images, _ = m.docker.ProjectImages(ctx, files.ProjectName)
	s.Images = res.Images
	if err := m.store(ctx, s); err != nil {
		return res, err
	}
	_ = m.nodeQ.SetContainerFilesRoot(ctx, setFilesRoot(s, filesRoot))
	for _, r := range s.GetRoutes() {
		onLine("对外开放：" + spec.RouteLabel(r))
	}
	return res, nil
}

// writeOverride 生成并写入容器的叠加配置。
func (m *Manager) writeOverride(ctx context.Context, s *nagiv1.ContainerSpec, cp *types.Project) error {
	in := overrideInput{
		proj: cp, mp: containerPaths{root: s.GetDir()}, projectID: s.GetProjectId(), container: s.GetName(), version: s.GetVersion(),
		single: spec.IsSingle(s.GetConfig()), pins: s.GetImages(), senv: spec.ServiceEnv(s.GetConfig(), s.GetName()),
		managedRestart: m.ManagedRestart != nil && m.ManagedRestart(s.GetProjectId(), s.GetName()),
	}
	if in.single {
		if err := m.ensureProjectNetwork(ctx, s.GetProjectId()); err != nil {
			return fmt.Errorf("创建项目内网: %w", err)
		}
	} else {
		subnet, err := m.subnetFor(ctx, groupOwner(s.GetProjectId(), s.GetName()), groupSubnetBits)
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

// Start 按缓存里部署过的版本重新生成配置文件和叠加配置并启动，不生成新版本，镜像固定到当时的摘要。
// 用于恢复快照后、顶替接手时（目录、网段可能变了，叠加配置要重写）。
func (m *Manager) Start(ctx context.Context, s *nagiv1.ContainerSpec, onLine func(string)) error {
	if s.GetVersion() == 0 {
		return nil
	}
	mp := containerPaths{root: s.GetDir()}
	cp, err := spec.Validate(ctx, proto.Clone(s.GetConfig()).(*nagiv1.ContainerConfig), s.GetName(), s.GetSharedEnv(), mp.dataDir(), spec.ComposeProjectName(s.GetProjectId(), s.GetName()))
	if err != nil {
		return err
	}
	if err := writeSecrets(mp, s); err != nil {
		return err
	}
	if err := writeConfigFiles(mp, s.GetConfig(), s.GetName(), s.GetSharedEnv()); err != nil {
		return err
	}
	if err := m.writeOverride(ctx, s, cp); err != nil {
		return err
	}
	if err := compose.Up(ctx, composeFiles(s), onLine); err != nil {
		return err
	}
	return m.SyncNetworks(ctx, s.GetProjectId())
}

// findVersionFiles 找到某个版本保存的文件（版本文件快照）。
func findVersionFiles(ctx context.Context, rep *snapshot.Repo, version int64) (string, error) {
	list, err := rep.List(ctx, snapshot.Version)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.Tags["version"] == strconv.FormatInt(version, 10) {
			return s.ID, nil
		}
	}
	return "", invalid("版本 %d 的文件已经不在这个节点上（可能已被清理，或者容器换过节点），不能回滚到这个版本", version)
}

// findPreDeploy 找到某个版本被替换前保存的运行数据（部署前快照）。
func findPreDeploy(ctx context.Context, rep *snapshot.Repo, version int64) (string, error) {
	list, err := rep.List(ctx, snapshot.PreDeploy)
	if err != nil {
		return "", err
	}
	for _, s := range list { // 最新的在前
		if s.Tags["from_version"] == strconv.FormatInt(version, 10) {
			return s.ID, nil
		}
	}
	return "", invalid("没有找到版本 %d 被替换前的数据快照，无法同时恢复数据", version)
}

func preserveIgnore(preserve []string) []string {
	var out []string
	for _, p := range preserve {
		out = append(out, "/data/"+p)
	}
	return out
}
