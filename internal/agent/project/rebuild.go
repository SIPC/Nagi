package project

import (
	"context"
	"fmt"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 用面板的配置备份重建项目（见 docs/failover.md#用配置备份重建项目）：所在节点坏了、又没有副本时的最后手段。
// 建成一个新项目（新 ID，容器的 deploy key 和 webhook 令牌也是新的），原节点回来时两份互不干扰。
// 配置备份里只有每个容器最新版本的配置和草稿，没有项目文件、运行数据和历史版本。

// Rebuild 按配置备份 src 在本节点的 dir 建出一个新项目，部署能直接部署的容器，返回新项目的 ID。
// 项目建好以后部署失败时，项目保留（返回 ID 和错误），修好后手动部署。
func (m *Manager) Rebuild(ctx context.Context, src *nagiv1.Project, dir string, onLine func(string)) (string, error) {
	if src == nil {
		return "", invalid("缺少配置备份")
	}
	settings := proto.Clone(src.GetSettings()).(*nagiv1.ProjectSettings)
	if settings == nil {
		settings = &nagiv1.ProjectSettings{}
	}
	proj, err := m.Create(ctx, src.GetName(), dir, settings, "", nil)
	if err != nil {
		return "", err
	}
	id := proj.GetId()
	onLine(fmt.Sprintf("已建好新项目 %s（ID %s），目录 %s", src.GetName(), id, proj.GetDir()))
	if err := m.rebuildConfig(ctx, id, src, settings, onLine); err != nil {
		// 配置都没建完的项目没有用处，删掉（目录是刚建的）。
		_ = m.Delete(context.WithoutCancel(ctx), id, 0, true, src.GetName(), func(string) {})
		return "", err
	}

	// 能直接部署的容器：从 git 拉文件的，或者不用 data/ 里文件的。
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return id, err
	}
	var jobs []job
	var waiting []string
	for _, mem := range src.GetContainers() {
		cfg := mem.GetConfig()
		if cfg.GetSource() == nil && m.usesDataFiles(ctx, cfg, mem.GetName(), paths{root: row.Dir}, id, settings) {
			waiting = append(waiting, mem.GetName())
			continue
		}
		mrow, err := pdb.q.GetContainer(ctx, mem.GetName())
		if err != nil {
			unlock()
			pdb.Close()
			return id, err
		}
		jobs = append(jobs, job{row: mrow, cfg: cfg, opts: containerOpts{pull: cfg.GetSource() != nil}})
	}
	if len(waiting) > 0 {
		onLine("这些容器要用 data/ 里的文件，没有自动部署，请先上传文件再部署：" + strings.Join(waiting, "、"))
	}
	var deployErr error
	if len(jobs) > 0 {
		_, deployErr = m.runJobs(ctx, row, pdb, id, jobs, "manual", 0, func(job) {}, onLine)
	}
	unlock()
	pdb.Close()
	if err := m.SyncNetworks(ctx, id); err != nil {
		onLine("警告：调整网络连接失败：" + err.Error())
	}

	// 草稿最后放回去：部署会用掉容器的草稿。
	if err := m.rebuildDrafts(ctx, id, src); err != nil {
		onLine("警告：恢复草稿失败：" + err.Error())
	}
	var git []string
	for _, mem := range src.GetContainers() {
		if mem.GetConfig().GetSource() != nil {
			git = append(git, mem.GetName())
		}
	}
	if len(git) > 0 {
		onLine("这些容器的 deploy key 是新的，私有仓库要先把新的 deploy key 加进去再部署：" + strings.Join(git, "、"))
	}
	onLine("所有容器的 webhook 地址都换了，用到的地方（例如 git 仓库）要更新")
	return id, deployErr
}

// rebuildConfig 加容器、恢复项目设置和端口映射。
func (m *Manager) rebuildConfig(ctx context.Context, id string, src *nagiv1.Project, settings *nagiv1.ProjectSettings, onLine func(string)) error {
	for _, mem := range src.GetContainers() {
		if mem.GetConfig() == nil {
			return invalid("配置备份里容器 %s 没有配置", mem.GetName())
		}
		if _, err := m.AddContainer(ctx, id, mem.GetName(), mem.GetConfig()); err != nil {
			return fmt.Errorf("加容器 %s：%w", mem.GetName(), err)
		}
		onLine("已加容器 " + mem.GetName())
	}
	// 网络名、共享网络和跨节点端口：Create 不带共享网络，这里直接写进设置，再按迁移过来的规则退出本节点上用不了的网络。
	_, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return err
	}
	cur := pdb.settings(ctx)
	cur.NetworkName, cur.SharedNetworks, cur.LinkPorts = settings.GetNetworkName(), settings.GetSharedNetworks(), settings.GetLinkPorts()
	err = pdb.setSettings(ctx, cur)
	if err == nil {
		err = m.leaveForeignNetworks(ctx, pdb, id, onLine)
	}
	if err == nil && src.GetHasSharedEnvDraft() {
		var raw []byte
		if raw, err = protojson.Marshal(&nagiv1.EnvList{Vars: src.GetSharedEnvDraft()}); err == nil {
			err = pdb.setMeta(ctx, metaSharedEnvDraft, string(raw))
		}
	}
	unlock()
	pdb.Close()
	if err != nil {
		return err
	}
	for _, r := range src.GetRoutes() {
		if _, err := m.SaveRoute(ctx, id, r); err != nil {
			onLine(fmt.Sprintf("跳过端口映射 %s（%s:%d）：%v", r.GetName(), r.GetService(), r.GetTarget(), err))
		}
	}
	return nil
}

// rebuildDrafts 放回配置备份里容器的草稿。
func (m *Manager) rebuildDrafts(ctx context.Context, id string, src *nagiv1.Project) error {
	for _, mem := range src.GetContainers() {
		if !mem.GetHasDraft() || mem.GetDraft() == nil {
			continue
		}
		if _, err := m.SaveDraft(ctx, id, mem.GetName(), 0, mem.GetDraft()); err != nil {
			return fmt.Errorf("容器 %s：%w", mem.GetName(), err)
		}
	}
	return nil
}

// usesDataFiles 判断容器是否要用 data/ 里的文件（镜像表单设了工作目录，或者 compose 挂载了 data/ 下的路径）。
func (m *Manager) usesDataFiles(ctx context.Context, cfg *nagiv1.ContainerConfig, container string, p paths, id string, settings *nagiv1.ProjectSettings) bool {
	mp := p.container(container)
	cp, err := loadContainer(ctx, cfg, container, settings.GetSharedEnv(), mp.dataDir(), composeProjectName(id, container))
	if err != nil {
		return false
	}
	return len(dataServices(cp, mp.dataDir())) > 0
}
