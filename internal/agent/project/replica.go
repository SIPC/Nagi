package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 副本（见 docs/snapshot.md#备份节点的实现）：只同步容器的快照仓库 backups/，配置由面板下发。
// 这里负责源节点上的文件清单、迁移时的停止和撤销，以及在目标节点上把副本恢复成容器；传输由节点经 mesh 完成。

// ReplicaFile 是要同步到备份节点的一个文件。
type ReplicaFile struct {
	Path string // 副本里的相对路径：repo/...
	Abs  string
	Size int64
	// Immutable 表示内容写入后不会再变，大小相同就不用再传。kopia 仓库里的文件名都由内容决定、写入后不变，
	// 只有 kopia.* 开头的几个（例如维护进度 kopia.maintenance）会被改写。
	Immutable bool
}

// ReplicaFiles 返回容器快照仓库里要同步的文件。先数据包、再其他文件：仓库的索引写在数据包之后，
// 按这个顺序传，备份节点上的仓库始终能打开。
func (m *Manager) ReplicaFiles(ctx context.Context, projectID, container string) ([]ReplicaFile, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return nil, err
	}
	dir := containerPaths{root: c.Spec.GetDir()}.backupsDir()
	var packs, others []ReplicaFile
	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil // 维护时可能正在删除
		}
		rel, _ := filepath.Rel(dir, full)
		b := d.Name()
		f := ReplicaFile{Path: "repo/" + filepath.ToSlash(rel), Abs: full, Size: info.Size(), Immutable: !strings.HasPrefix(b, "kopia.")}
		if strings.HasPrefix(b, "p") || strings.HasPrefix(b, "q") {
			packs = append(packs, f)
		} else {
			others = append(others, f)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取快照仓库: %w", err)
	}
	return append(packs, others...), nil
}

// ReplicaPath 校验副本里的相对路径（只能在 repo/ 下），返回 root 下的绝对路径。
func ReplicaPath(root, rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean != rel || filepath.IsAbs(rel) || !strings.HasPrefix(clean, "repo/") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("副本路径无效：%q", rel)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// ReplicaSnapshots 列出副本里的快照。repoDir 是副本的快照仓库，stateDir 是副本自己的连接配置和缓存目录。
func ReplicaSnapshots(ctx context.Context, s *nagiv1.ContainerSpec, repoDir, stateDir string) ([]*nagiv1.Snapshot, error) {
	rep, err := snapshot.Open(ctx, uid(s.GetProjectId(), s.GetName()), repoDir, s.GetBackupPassword(), stateDir)
	if err != nil {
		return nil, fmt.Errorf("副本还没有同步完成：%w", err)
	}
	defer rep.Close(ctx)
	list, err := rep.List(ctx)
	if err != nil {
		return nil, err
	}
	return snapshotsProto(list, s.GetName()), nil
}

// CheckPlacement 检查容器能不能放到本节点上：本节点上还没有它，目录可用，端口没被其他程序占用。不做任何改动。
func (m *Manager) CheckPlacement(ctx context.Context, s *nagiv1.ContainerSpec) error {
	if err := checkSpec(s); err != nil {
		return err
	}
	if _, err := m.Get(ctx, s.GetProjectId(), s.GetName()); err == nil {
		return invalid("目标节点上已经有容器 %s", s.GetName())
	}
	if _, err := m.CheckDir(ctx, s.GetDir()); err != nil {
		return err
	}
	return m.checkNewPorts(ctx, s)
}

// RestoreReplica 把副本恢复成本节点上的容器：复制快照仓库，恢复快照里的 data/ 和 volumes/
// （还没有定时或手动快照时，用部署的版本的文件恢复 data/），然后按 s 里部署过的版本启动，不生成新版本。
// snapshotID 为空时用最新的定时或手动快照。文件恢复好以后启动失败，容器保留在本节点上，返回错误。
func (m *Manager) RestoreReplica(ctx context.Context, s *nagiv1.ContainerSpec, repoDir, snapshotID string, onLine func(string)) error {
	if err := checkSpec(s); err != nil {
		return err
	}
	l := m.lock(s.GetProjectId(), s.GetName())
	l.Lock()
	defer l.Unlock()
	if _, err := m.Get(ctx, s.GetProjectId(), s.GetName()); err == nil {
		return invalid("本节点上已经有容器 %s", s.GetName())
	}
	dir, err := m.CheckDir(ctx, s.GetDir())
	if err != nil {
		return err
	}
	s = proto.Clone(s).(*nagiv1.ContainerSpec)
	s.Dir = dir
	mp := containerPaths{root: dir}
	fail := func(err error) error {
		_ = m.nodeQ.DeleteContainer(ctx, nodestore.DeleteContainerParams{ProjectID: s.GetProjectId(), Name: s.GetName()})
		os.RemoveAll(filepath.Join(m.stateDir, uid(s.GetProjectId(), s.GetName())))
		os.RemoveAll(dir)
		return err
	}
	onLine("复制快照仓库")
	if err := copyTree(repoDir, mp.backupsDir()); err != nil {
		return fail(fmt.Errorf("复制副本: %w", err))
	}
	if err := m.prepareDir(mp); err != nil {
		return fail(err)
	}
	rep, err := m.openRepo(ctx, s)
	if err != nil {
		return fail(fmt.Errorf("打开快照仓库: %w", err))
	}
	err = restoreData(ctx, rep, s, mp, snapshotID, onLine)
	rep.Close(ctx)
	if err != nil {
		return fail(err)
	}
	// 文件已经恢复好：缓存这份配置，之后启动失败容器也留在本节点上，用户可以修改配置后重新部署。
	if err := m.store(ctx, s); err != nil {
		return fail(err)
	}
	if s.GetVersion() == 0 {
		onLine("容器还没有部署过，不启动")
		return nil
	}
	onLine(fmt.Sprintf("启动容器 %s（版本 %d）", s.GetName(), s.GetVersion()))
	if err := m.Start(ctx, s, onLine); err != nil {
		return fmt.Errorf("容器已恢复，但启动失败：%w", err)
	}
	return nil
}

// restoreData 从快照仓库恢复容器的 data/ 和 volumes/。
func restoreData(ctx context.Context, rep *snapshot.Repo, s *nagiv1.ContainerSpec, mp containerPaths, sid string, onLine func(string)) error {
	if sid == "" {
		list, err := rep.List(ctx, snapshot.Scheduled, snapshot.Manual)
		if err != nil {
			return err
		}
		if len(list) > 0 {
			sid = list[0].ID
			onLine(fmt.Sprintf("使用最新的快照（%s）", list[0].Time.Local().Format("2006-01-02 15:04:05")))
		}
	} else if tags, err := rep.Tags(ctx, sid); err != nil {
		return invalid("快照不存在：%v", err)
	} else if k := snapshot.Kind(tags["kind"]); k != snapshot.Scheduled && k != snapshot.Manual {
		return invalid("只能从定时快照或手动快照恢复")
	}
	if sid == "" {
		// 还没有定时快照（例如容器刚设置备份节点）：用部署的版本的文件恢复 data/，没有运行数据。
		if s.GetVersion() == 0 {
			return nil
		}
		vid, err := findVersionFiles(ctx, rep, s.GetVersion())
		if err != nil {
			onLine("副本里没有快照，也没有版本文件，data/ 和 volumes/ 为空")
			return nil
		}
		onLine("副本里还没有定时快照，用版本文件恢复 data/（没有运行数据）")
		return rep.Restore(ctx, vid, "data", mp.dataDir())
	}
	onLine("恢复 data/ 和 volumes/")
	for _, d := range dataDirs(mp) {
		if err := removeSymlinks(d.Path); err != nil {
			return err
		}
		if err := rep.Restore(ctx, sid, d.Name, d.Path); err != nil {
			return fmt.Errorf("恢复 %s: %w", d.Name, err)
		}
	}
	return nil
}

// MigrateStop 是迁移的停止阶段：停止并移除容器（命名卷都绑定在容器目录里，删除卷对象不会删数据），
// 再保存最后一次快照，之后由节点做最后一次增量同步。
func (m *Manager) MigrateStop(ctx context.Context, projectID, container string, onLine func(string)) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	onLine("停止容器 " + container)
	if err := m.down(ctx, c.Spec, onLine); err != nil {
		return err
	}
	rep, err := m.openRepo(ctx, c.Spec)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	onLine("保存最后一次快照")
	if _, err := rep.Create(ctx, snapshot.Manual, dataDirs(containerPaths{root: c.Spec.GetDir()}), snapshotIgnore, map[string]string{"container": container, "migrate": "1"}); err != nil {
		return fmt.Errorf("保存快照: %w", err)
	}
	return nil
}

// MigrateResume 撤销迁移：按缓存的配置重新启动容器。
func (m *Manager) MigrateResume(ctx context.Context, projectID, container string, onLine func(string)) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	onLine("重新启动容器 " + container)
	return m.Start(ctx, c.Spec, onLine)
}

// Retire 处理已经在别的节点上恢复的容器（原节点离线时恢复或重新部署的）：停止并删除它，
// 目录改名为 <目录>.replaced-<时间> 保留，由用户决定要不要从里面找回数据。
func (m *Manager) Retire(ctx context.Context, projectID, container string, onLine func(string)) (string, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return "", err
	}
	if err := m.Delete(ctx, projectID, container, false, onLine); err != nil {
		return "", err
	}
	dir := c.Spec.GetDir()
	dest := dir + ".replaced-" + time.Now().Format("20060102-150405")
	if err := os.Rename(dir, dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("目录改名: %w", err)
	}
	return dest, nil
}

// copyTree 把 src 下的普通文件复制到 dst（同名覆盖）。
func copyTree(src, dst string) error {
	var files []string
	err := filepath.WalkDir(src, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, full)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(src, f)
		info, err := os.Stat(f)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFile(f, target, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
