package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 快照（见 docs/snapshot.md）：每个容器一个 kopia 仓库，在容器目录的 backups/ 里。
//   - 定时和手动快照：容器的 data/ 和 volumes/；
//   - 部署前快照：有状态容器每次部署前保存一份运行数据，回滚时可以一起恢复；
//   - 版本文件：每次部署时 data/ 中除保留路径以外的内容，回滚时用。

const (
	pauseLimit          = 30 * time.Second
	defaultSnapshotGap  = 60 * time.Minute
	failoverSnapshotGap = 15 * time.Minute
	maintainGap         = 24 * time.Hour
)

var snapshotIgnore = []string{"/data/.checkout/", "/data/.restore/"}

func (m *Manager) openRepo(ctx context.Context, s *nagiv1.ContainerSpec) (*snapshot.Repo, error) {
	id := uid(s.GetProjectId(), s.GetName())
	return snapshot.Open(ctx, id, containerPaths{root: s.GetDir()}.backupsDir(), s.GetBackupPassword(), filepath.Join(m.stateDir, id))
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

func dataDirs(mp containerPaths) []snapshot.Dir {
	return []snapshot.Dir{{Name: "data", Path: mp.dataDir()}, {Name: "volumes", Path: mp.volumesDir()}}
}

// TakeSnapshot 给容器做一份定时或手动快照：data/ 和 volumes/。有状态的容器快照时暂停。
func (m *Manager) TakeSnapshot(ctx context.Context, projectID, container string, kind snapshot.Kind, onLine func(string)) (string, error) {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	return m.takeSnapshot(ctx, projectID, container, kind, nil, onLine)
}

func (m *Manager) takeSnapshot(ctx context.Context, projectID, container string, kind snapshot.Kind, tags map[string]string, onLine func(string)) (string, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return "", err
	}
	if c.Spec.GetVersion() == 0 {
		return "", invalid("容器 %s 还没有部署过", container)
	}
	rep, err := m.openRepo(ctx, c.Spec)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)
	sid, err := m.snapshotWithPause(ctx, rep, containerLabel(projectID, container), kind, dataDirs(containerPaths{root: c.Spec.GetDir()}),
		snapshotIgnore, tags, c.Spec.GetStateful(), onLine)
	if err != nil {
		return "", err
	}
	_ = m.nodeQ.SetContainerSnapshotTime(ctx, nodestore.SetContainerSnapshotTimeParams{LastSnapshot: time.Now().Unix(), ProjectID: projectID, Name: container})
	onLine("快照已保存")
	return sid, nil
}

// SnapshotDue 给到了间隔时间、部署过的容器做定时快照。由节点每分钟调用一次。
func (m *Manager) SnapshotDue(ctx context.Context, onResult func(projectID, container string, err error)) {
	for _, c := range m.List(ctx) {
		s := c.Spec
		if s.GetVersion() == 0 {
			continue
		}
		gap := defaultSnapshotGap
		if n := s.GetSnapshotIntervalMinutes(); n > 0 {
			gap = time.Duration(n) * time.Minute
		}
		// 设置了顶替的容器每 15 分钟一次（与同步给热备的频率一致），除非用户设得更短。
		if m.FastSnapshot != nil && m.FastSnapshot(s.GetProjectId(), s.GetName()) && gap > failoverSnapshotGap {
			gap = failoverSnapshotGap
		}
		if time.Since(c.LastSnapshot) < gap {
			continue
		}
		_, err := m.TakeSnapshot(ctx, s.GetProjectId(), s.GetName(), snapshot.Scheduled, func(string) {})
		if err == nil && time.Since(c.LastMaintain) >= maintainGap {
			if err = m.maintain(ctx, s); err != nil {
				err = fmt.Errorf("快照仓库维护: %w", err)
			}
		}
		if onResult != nil {
			onResult(s.GetProjectId(), s.GetName(), err)
		}
	}
}

// maintain 运行快照仓库维护，回收过期快照占用的空间。每个容器每天一次。
func (m *Manager) maintain(ctx context.Context, s *nagiv1.ContainerSpec) error {
	l := m.lock(s.GetProjectId(), s.GetName())
	l.Lock()
	defer l.Unlock()
	rep, err := m.openRepo(ctx, s)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	if err := rep.Maintain(ctx, false); err != nil {
		return err
	}
	return m.nodeQ.SetContainerMaintenanceTime(ctx, nodestore.SetContainerMaintenanceTimeParams{
		LastMaintenance: time.Now().Unix(), ProjectID: s.GetProjectId(), Name: s.GetName(),
	})
}

// RestoreSnapshot 从定时或手动快照恢复容器的 data/ 和 volumes/：先停止、恢复，再重新启动。配置不变。
func (m *Manager) RestoreSnapshot(ctx context.Context, projectID, container, sid string, onLine func(string)) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	if c.Spec.GetVersion() == 0 {
		return invalid("容器 %s 还没有部署过", container)
	}
	rep, err := m.openRepo(ctx, c.Spec)
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
	files := composeFiles(c.Spec)
	mp := containerPaths{root: c.Spec.GetDir()}
	onLine("停止容器 " + container)
	if err := compose.Stop(ctx, files, onLine); err != nil {
		return err
	}
	onLine("恢复 data/ 和 volumes/")
	for _, d := range dataDirs(mp) {
		// 恢复是原地覆盖的，先删掉容器可能留下的符号链接（见 removeSymlinks）。
		if err := removeSymlinks(d.Path); err != nil {
			return err
		}
		if err := rep.Restore(ctx, sid, d.Name, d.Path); err != nil {
			return fmt.Errorf("恢复 %s: %w", d.Name, err)
		}
	}
	onLine("重新启动容器 " + container)
	if err := compose.Up(ctx, files, onLine); err != nil {
		return err
	}
	_ = m.SyncNetworks(ctx, projectID)
	onLine("已从快照恢复")
	return nil
}

// Snapshots 列出容器的快照，最新的在前。
func (m *Manager) Snapshots(ctx context.Context, projectID, container string) ([]*nagiv1.Snapshot, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return nil, err
	}
	rep, err := m.openRepo(ctx, c.Spec)
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)
	list, err := rep.List(ctx)
	if err != nil {
		return nil, err
	}
	return snapshotsProto(list, container), nil
}

func snapshotsProto(list []snapshot.Info, container string) []*nagiv1.Snapshot {
	out := make([]*nagiv1.Snapshot, 0, len(list))
	for _, s := range list {
		snap := &nagiv1.Snapshot{Id: s.ID, Kind: string(s.Kind), Time: timestamppb.New(s.Time), Size: s.Size, Container: container}
		v := s.Tags["version"]
		if s.Kind == snapshot.PreDeploy {
			v = s.Tags["from_version"]
		}
		snap.Version, _ = strconv.ParseInt(v, 10, 64)
		out = append(out, snap)
	}
	return out
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
