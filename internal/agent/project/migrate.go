package project

import (
	"context"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/snapshot"
)

// 手动迁移（见 docs/failover.md#手动迁移）：源节点一侧。
//   1. 项目照常运行，先同步一次到目标节点（由节点经 mesh 完成）；
//   2. MigrateStop：停止并移除所有成员的容器，保存最后一次快照；
//   3. 再增量同步一次，目标节点恢复并启动；
//   4. 成功则 MigrateFinish 把项目移出本节点（目录保留），失败则 MigrateResume 原地恢复运行。
// 停止以后项目标记为"迁移中"，期间不能部署或修改，避免迁移过程中被重新启动。

const metaMigrating = "migrating"

// MigrateStop 停止并移除项目所有成员的容器（数据保留），删除项目内网，保存最后一次快照，返回快照 ID。
func (m *Manager) MigrateStop(ctx context.Context, id string, onLine func(string)) (string, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return "", err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.setMeta(ctx, metaMigrating, "1"); err != nil {
		return "", err
	}
	p := paths{root: row.Dir}
	members, err := memberNames(ctx, pdb)
	if err != nil {
		return "", err
	}
	onLine("停止并移除容器（数据保留）")
	for _, name := range members {
		if err := m.down(ctx, id, p, name, onLine); err != nil {
			return "", err
		}
	}
	_ = m.docker.RemoveNetwork(ctx, projectNetwork(id))
	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)
	onLine("保存最后一次数据快照")
	sid, err := rep.Create(ctx, snapshot.Manual, snapshotDirs(p, members), snapshotIgnore, map[string]string{"migration": "1"})
	if err != nil {
		return "", err
	}
	_ = pdb.setMeta(ctx, metaLastSnapshot, strconv.FormatInt(time.Now().Unix(), 10))
	return sid, nil
}

// MigrateResume 取消迁移，在本节点上重新启动项目。
func (m *Manager) MigrateResume(ctx context.Context, id string, onLine func(string)) error {
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
	if err := pdb.setMeta(ctx, metaMigrating, ""); err != nil {
		return err
	}
	onLine("在原节点上重新启动项目")
	return m.upAll(ctx, pdb, paths{root: row.Dir}, id, onLine)
}

// upAll 启动项目所有部署过的成员（容器还在时就是启动，不在时按当前文件创建），再接好网络。
func (m *Manager) upAll(ctx context.Context, pdb *projectDB, p paths, id string, onLine func(string)) error {
	members, err := pdb.q.ListMembers(ctx)
	if err != nil {
		return err
	}
	settings := pdb.settings(ctx)
	for _, mem := range members {
		if mem.DeployedVersion == 0 {
			continue
		}
		cfg, err := pdb.current(ctx, mem)
		if err == nil && isSingle(cfg) && !settings.GetIntranetDisabled() {
			if err := m.ensureProjectNetwork(ctx, id); err != nil {
				return err
			}
		}
		if err := compose.Up(ctx, m.composeFiles(id, p, mem.Name), onLine); err != nil {
			return err
		}
	}
	return m.SyncNetworks(ctx, id)
}

// MigrateFinish 迁移成功后把项目移出本节点。容器已经移除，项目目录保留在磁盘上，由用户确认后自己删除。
func (m *Manager) MigrateFinish(ctx context.Context, id string) (string, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return "", err
	}
	if err := m.releaseProject(ctx, id); err != nil {
		return "", err
	}
	return row.Dir, nil
}
