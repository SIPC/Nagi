package project

import (
	"context"
	"slices"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 项目设置（见 docs/agent.md#修改与部署）：名称、内网、共享网络、快照间隔不进容器的版本，改了立即生效；
// 共享变量走草稿，部署时写进 config/.env，并记进部署的容器的版本。

// UpdateSettings 修改项目设置，立即生效。settings.shared_env 在这里不生效（共享变量走草稿）。
// 开关内网时单个容器要重建（它的网络变了），容器组只接入或断开项目内网。
func (m *Manager) UpdateSettings(ctx context.Context, id string, baseRevision int64, name string, in *nagiv1.ProjectSettings, onLine func(string)) (*nagiv1.Project, error) {
	if in == nil {
		in = &nagiv1.ProjectSettings{}
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, invalid("项目名不能为空，且不超过 64 个字符")
	}
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.checkRevision(ctx, baseRevision); err != nil {
		return nil, err
	}
	old := pdb.settings(ctx)
	next := proto.Clone(old).(*nagiv1.ProjectSettings)
	next.IntranetDisabled = in.GetIntranetDisabled()
	next.NetworkName = strings.TrimSpace(in.GetNetworkName())
	next.SharedNetworks = nil
	for _, n := range in.GetSharedNetworks() {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(next.SharedNetworks, n) {
			next.SharedNetworks = append(next.SharedNetworks, n)
		}
	}
	next.SnapshotIntervalMinutes = in.GetSnapshotIntervalMinutes()
	if next.GetNetworkName() != "" && !nameRe.MatchString(next.GetNetworkName()) {
		return nil, invalid("网络名只能用小写字母、数字和 -，不超过 32 个字符")
	}
	if next.LinkPorts, err = cleanLinkPorts(in.GetLinkPorts()); err != nil {
		return nil, err
	}
	if err := m.checkSharedSettings(ctx, id, next); err != nil {
		return nil, err
	}
	// 新加入的共享网络（或者改了网络名）要检查相连节点上的网络名。
	var check []string
	for _, n := range next.GetSharedNetworks() {
		if old.GetNetworkName() != next.GetNetworkName() || !slices.Contains(old.GetSharedNetworks(), n) {
			check = append(check, n)
		}
	}
	if err := m.checkRemoteNames(ctx, id, next.GetNetworkName(), check, true); err != nil {
		return nil, err
	}
	if err := pdb.setSettings(ctx, next); err != nil {
		return nil, err
	}
	if err := pdb.setMeta(ctx, metaName, name); err != nil {
		return nil, err
	}
	if err := pdb.bumpRevision(ctx); err != nil {
		return nil, err
	}
	p := paths{root: row.Dir}
	if old.GetIntranetDisabled() != next.GetIntranetDisabled() {
		if !next.GetIntranetDisabled() {
			if err := m.ensureProjectNetwork(ctx, id); err != nil {
				return nil, err
			}
		}
		// 单个容器的网络写在叠加配置里，换网络要重建容器。
		containers, err := pdb.q.ListContainers(ctx)
		if err != nil {
			return nil, err
		}
		for _, mem := range containers {
			v, err := pdb.deployed(ctx, mem)
			if err != nil || v == nil {
				continue
			}
			cfg, err := unmarshalConfig(v.Config)
			if err != nil || !isSingle(cfg) {
				continue
			}
			onLine("重建单个容器 " + mem.Name + "（网络变了）")
			if err := m.startDeployed(ctx, pdb, p, id, mem, onLine); err != nil {
				return nil, err
			}
		}
	}
	if err := m.SyncNetworks(ctx, id); err != nil {
		onLine("警告：调整网络连接失败：" + err.Error())
	}
	if next.GetIntranetDisabled() {
		_ = m.docker.RemoveNetwork(ctx, projectNetwork(id))
	}
	return pdb.project(ctx, id, row.Dir)
}

// SaveSharedEnv 保存共享变量的草稿，返回引用了改动变量的容器（部署共享变量时默认一起部署）。
func (m *Manager) SaveSharedEnv(ctx context.Context, id string, baseRevision int64, vars []*nagiv1.EnvVar) (*nagiv1.Project, []string, error) {
	if err := checkEnv(vars); err != nil {
		return nil, nil, err
	}
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.checkRevision(ctx, baseRevision); err != nil {
		return nil, nil, err
	}
	raw, err := protojson.Marshal(&nagiv1.EnvList{Vars: vars})
	if err != nil {
		return nil, nil, err
	}
	if err := pdb.setMeta(ctx, metaSharedEnvDraft, string(raw)); err != nil {
		return nil, nil, err
	}
	if err := pdb.bumpRevision(ctx); err != nil {
		return nil, nil, err
	}
	affected, err := affectedContainers(ctx, pdb, pdb.settings(ctx).GetSharedEnv(), vars)
	if err != nil {
		return nil, nil, err
	}
	proj, err := pdb.project(ctx, id, row.Dir)
	return proj, affected, err
}

// affectedContainers 返回引用了 before 和 after 之间有变化的共享变量的容器。
func affectedContainers(ctx context.Context, pdb *projectDB, before, after []*nagiv1.EnvVar) ([]string, error) {
	rows, err := pdb.q.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		cfg, err := pdb.current(ctx, r)
		if err != nil {
			continue
		}
		if d, err := pdb.draft(ctx, r.Name); err == nil && d != nil {
			cfg = d
		}
		if sharedEnvChanged(cfg, r.Name, before, after) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

// DiscardSharedEnv 放弃共享变量的草稿。
func (m *Manager) DiscardSharedEnv(ctx context.Context, id string, baseRevision int64) (*nagiv1.Project, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.checkRevision(ctx, baseRevision); err != nil {
		return nil, err
	}
	if err := pdb.setMeta(ctx, metaSharedEnvDraft, ""); err != nil {
		return nil, err
	}
	if err := pdb.bumpRevision(ctx); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// applySharedEnv 让共享变量的草稿生效：写进项目设置和 config/.env，删除草稿。之后部署的容器都用新的值。
func applySharedEnv(ctx context.Context, pdb *projectDB, p paths) error {
	d := pdb.sharedEnvDraft(ctx)
	if d == nil {
		return invalid("共享变量没有要部署的修改")
	}
	s := pdb.settings(ctx)
	s.SharedEnv = d.GetVars()
	if err := writeFileAtomic(p.sharedEnvFile(), envFile(s.GetSharedEnv()), 0o600); err != nil {
		return err
	}
	if err := pdb.setSettings(ctx, s); err != nil {
		return err
	}
	if err := pdb.setMeta(ctx, metaSharedEnvDraft, ""); err != nil {
		return err
	}
	return pdb.bumpRevision(ctx)
}
