package project

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/snapshot"
)

// 手动迁移（见 docs/failover.md#手动迁移计划内的顶替）：源节点一侧。
//   1. 项目照常运行，先同步一次到目标节点（由节点经 mesh 完成）；
//   2. MigrateStop：停止并移除容器，保存最后一次快照；
//   3. 再增量同步一次，目标节点恢复并启动；
//   4. 成功则 MigrateFinish 把项目移出本节点（目录保留），失败则 MigrateResume 原地恢复运行。
// 停止以后项目标记为"迁移中"，期间不能部署或修改，避免迁移过程中被重新启动。

const metaMigrating = "migrating"

// snapshotDirs 和 snapshotIgnore 是数据快照包含的目录和忽略的文件。
func snapshotDirs(p paths) []snapshot.Dir {
	return []snapshot.Dir{{Name: "data", Path: p.dataDir()}, {Name: "volumes", Path: p.volumesDir()}, {Name: "config", Path: p.configDir()}}
}

var snapshotIgnore = []string{
	"/config/.checkout/", "/config/.restore/", "/config/.upload/", "/config/.upload-*/",
	"/config/*.tmp", "/config/project.db-wal", "/config/project.db-shm",
}

// MigrateStop 停止并移除项目的容器（数据保留），保存最后一次快照，返回快照 ID。
func (m *Manager) MigrateStop(ctx context.Context, id string, onLine func(string)) (string, error) {
	row, pdb, _, unlock, err := m.projectForUpdate(ctx, id, 0)
	if err != nil {
		return "", err
	}
	defer unlock()
	defer pdb.Close()
	if err := pdb.setMeta(ctx, metaMigrating, "1"); err != nil {
		return "", err
	}
	p := paths{root: row.Dir}
	if _, err := os.Stat(p.overrideFile()); err == nil {
		onLine("停止并移除容器（数据保留）")
		// 命名卷都绑定在项目目录的 volumes/ 下，删除卷对象不会删数据；不留下指向本节点目录的卷对象。
		if err := compose.Down(ctx, composeFiles(id, p), true, onLine); err != nil {
			return "", err
		}
	}
	rep, err := m.openRepo(ctx, id, p)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)
	onLine("保存最后一次数据快照")
	sid, err := rep.Create(ctx, snapshot.Manual, snapshotDirs(p), snapshotIgnore, map[string]string{"migration": "1"})
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
	p := paths{root: row.Dir}
	if _, err := os.Stat(p.overrideFile()); err != nil {
		return nil // 从未部署过
	}
	onLine("在原节点上重新启动项目")
	return compose.Up(ctx, composeFiles(id, p), onLine)
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
	if err := m.nodeQ.DeleteProject(ctx, id); err != nil {
		return "", err
	}
	os.RemoveAll(filepath.Join(m.stateDir, id))
	return row.Dir, nil
}
