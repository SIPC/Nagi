package project

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 容器（见 docs/agent.md#项目与容器）：项目里独立部署的单位。容器名在项目内唯一，创建后不能改。

func checkContainerName(name string) error {
	if !nameRe.MatchString(name) {
		return invalid("容器名只能用小写字母、数字和 -，不以 - 开头或结尾，不超过 32 个字符")
	}
	return nil
}

// AddContainer 往项目里加一个容器：保存配置作为它的版本 1（还没部署）。
func (m *Manager) AddContainer(ctx context.Context, id, name string, cfg *nagiv1.ContainerConfig) (*nagiv1.Project, error) {
	if err := checkContainerName(name); err != nil {
		return nil, err
	}
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	err = m.addContainer(ctx, pdb, paths{root: row.Dir}, id, name, cfg)
	pdb.Close()
	if err != nil {
		return nil, err
	}
	return m.getProject(ctx, id)
}

func (m *Manager) addContainer(ctx context.Context, pdb *projectDB, p paths, id, name string, cfg *nagiv1.ContainerConfig) error {
	if cfg == nil {
		return invalid("缺少容器配置")
	}
	cfg = proto.Clone(cfg).(*nagiv1.ContainerConfig)
	if _, err := pdb.q.GetContainer(ctx, name); err == nil {
		return invalid("项目里已经有容器 %s", name)
	}
	mp := p.container(name)
	if entries, err := os.ReadDir(mp.root); err == nil && len(entries) > 0 {
		return invalid("容器目录 %s 已存在（可能是之前删除容器时保留的），换个名字或先删除这个目录", mp.root)
	}
	settings := pdb.settings(ctx)
	cp, err := validate(ctx, cfg, name, settings.GetSharedEnv(), mp.dataDir(), composeProjectName(id, name))
	if err != nil {
		return err
	}
	if err := checkNames(ctx, pdb, name, cfg, cp.ServiceNames()); err != nil {
		return err
	}
	for _, d := range []string{mp.dataDir(), mp.volumesDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建容器目录: %w", err)
		}
	}
	if _, err := ensureDeployKey(mp); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	if err := pdb.q.InsertContainer(ctx, projectstore.InsertContainerParams{Name: name, CreatedAt: time.Now().Unix(), WebhookToken: token}); err != nil {
		return err
	}
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := pdb.q.InsertVersion(ctx, projectstore.InsertVersionParams{
		Container: name, Version: 1, Config: data, SharedEnv: marshalEnv(settings.GetSharedEnv()), CreatedAt: time.Now().Unix(),
	}); err != nil {
		return err
	}
	return writeConfigFiles(mp, cfg, name, settings.GetSharedEnv())
}

// checkNames 检查名字会不会撞：单个容器在项目内网里的名字就是容器名，容器组内部按服务原名互相访问，
// 所以单个容器的名字不能和项目里任何 容器组的服务名相同，否则 容器组内部查这个名字时
// 可能查到项目内网里的单个容器。container 是正在加入或修改的容器，services 是它的服务。
func checkNames(ctx context.Context, pdb *projectDB, container string, cfg *nagiv1.ContainerConfig, services []string) error {
	rows, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return err
	}
	settings := pdb.settings(ctx)
	for _, r := range rows {
		if r.Name == container {
			continue
		}
		var cfgs []*nagiv1.ContainerConfig
		if c, _, err := pdb.latest(ctx, r.Name); err == nil {
			cfgs = append(cfgs, c)
		}
		if d, err := pdb.draft(ctx, r.Name); err == nil && d != nil {
			cfgs = append(cfgs, d)
		}
		for _, other := range cfgs {
			switch {
			case isSingle(cfg) && !isSingle(other):
				cp, err := loadContainer(ctx, other, r.Name, settings.GetSharedEnv(), "/", "check")
				if err != nil {
					continue
				}
				if _, ok := cp.Services[container]; ok {
					return invalid("容器名 %s 和容器组 %s 里的一个容器同名：%s 内部查 %s 时可能查到这个容器，请换个名字", container, r.Name, r.Name, container)
				}
			case !isSingle(cfg) && isSingle(other):
				for _, s := range services {
					if s == r.Name {
						return invalid("这份 compose 里的 %s 和单个容器 %s 同名：compose 内部查 %s 时可能查到那个容器，请改 compose 里的服务名", s, r.Name, s)
					}
				}
			}
		}
	}
	return nil
}

// DeleteContainer 删除容器：停止并删除它的容器，从项目里移除（版本记录一并删除）。容器目录默认保留，
// deleteDir 时一起删除。对外映射里指向它的规则失效。
func (m *Manager) DeleteContainer(ctx context.Context, id, container string, baseRevision int64, deleteDir bool, onLine func(string)) (*nagiv1.Project, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.container(ctx, container)
	if err != nil {
		return nil, err
	}
	if err := checkContainerRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	p := paths{root: row.Dir}
	if err := m.down(ctx, id, p, container, onLine); err != nil {
		return nil, err
	}
	for _, f := range []func(context.Context, string) error{pdb.q.DeleteContainerVersions, pdb.q.DeleteDraft, pdb.q.DeleteContainer} {
		if err := f(ctx, container); err != nil {
			return nil, err
		}
	}
	_ = m.nodeQ.DeleteSubnet(ctx, groupOwner(id, container))
	if deleteDir {
		if err := os.RemoveAll(p.container(container).root); err != nil {
			return nil, fmt.Errorf("删除容器目录: %w", err)
		}
	}
	return pdb.project(ctx, id, row.Dir)
}

// SaveDraft 保存容器的草稿。草稿要先通过校验，部署时才会成为新版本。
func (m *Manager) SaveDraft(ctx context.Context, id, container string, baseRevision int64, cfg *nagiv1.ContainerConfig) (*nagiv1.Project, error) {
	if cfg == nil {
		return nil, invalid("缺少容器配置")
	}
	cfg = proto.Clone(cfg).(*nagiv1.ContainerConfig)
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.container(ctx, container)
	if err != nil {
		return nil, err
	}
	if err := checkContainerRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	latest, _, err := pdb.latest(ctx, container)
	if err != nil {
		return nil, err
	}
	if isSingle(latest) != isSingle(cfg) {
		return nil, invalid("容器创建后不能在单容器和 compose 之间切换，请新建一个容器")
	}
	mp := paths{root: row.Dir}.container(container)
	cp, err := validate(ctx, cfg, container, pdb.settings(ctx).GetSharedEnv(), mp.dataDir(), composeProjectName(id, container))
	if err != nil {
		return nil, err
	}
	if err := checkNames(ctx, pdb, container, cfg, cp.ServiceNames()); err != nil {
		return nil, err
	}
	if err := pdb.setDraft(ctx, container, cfg); err != nil {
		return nil, err
	}
	if err := pdb.bumpContainer(ctx, mrow); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// DiscardDraft 放弃容器的草稿。
func (m *Manager) DiscardDraft(ctx context.Context, id, container string, baseRevision int64) (*nagiv1.Project, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.container(ctx, container)
	if err != nil {
		return nil, err
	}
	if err := checkContainerRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	if err := pdb.q.DeleteDraft(ctx, container); err != nil {
		return nil, err
	}
	if err := pdb.bumpContainer(ctx, mrow); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// hasContainer 检查项目里有这个容器。
func (m *Manager) hasContainer(ctx context.Context, dir, container string) error {
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	_, err = pdb.container(ctx, container)
	return err
}

func marshalEnv(vars []*nagiv1.EnvVar) []byte {
	b, _ := proto.Marshal(&nagiv1.EnvList{Vars: vars})
	return b
}
