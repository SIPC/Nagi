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
	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/snapshot"
	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 快照（见 docs/snapshot.md）：每个容器一个 kopia 仓库，在容器目录的 backups/ 里。
//   - 定时和手动快照：容器的 data/ 和 volumes/；
//   - 部署前快照：有状态容器每次部署前保存一份运行数据，回滚时可以一起恢复；
//   - 版本文件：每次部署时 data/ 中除保留路径以外的内容，回滚时用。

const (
	pauseLimit          = 30 * time.Second
	failoverSnapshotGap = 15 * time.Minute
	maintainGap         = 24 * time.Hour
	// maintainRetry 是仓库维护失败以后多久再试。
	maintainRetry = time.Hour
	// 磁盘剩余空间低于这两个值之一时跳过定时快照，免得把磁盘写满（见 docs/snapshot.md#失败与退避）。
	minFreeBytes   = 2 << 30
	minFreePercent = 5
)

// snapshotBackoff 是定时快照连续失败以后的重试间隔：第 1、2、3 次及以后。不会超过快照间隔本身。
var snapshotBackoff = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// errNotDue：拿到锁以后再看，已经不用做了（例如刚做过一份手动快照）。
var errNotDue = errors.New("还没到时间")

var snapshotIgnore = []string{"/data/.checkout/", "/data/.restore/"}

func (m *Manager) openRepo(ctx context.Context, s *nagiv1.ContainerSpec) (*snapshot.Repo, error) {
	id := uid(s.GetProjectId(), s.GetName())
	rep, err := snapshot.Open(ctx, id, containerPaths{root: s.GetDir()}.backupsDir(), s.GetBackupPassword(), filepath.Join(m.stateDir, id))
	if err != nil {
		return nil, err
	}
	rep.Limit = int(spec.SnapshotLimit(s.GetSnapshotLimit()))
	return rep, nil
}

// snapshotWithPause 做一份快照。有状态时暂停 label 选出的容器，30 秒内没做完就放弃本次快照。
// 到 30 秒就恢复容器，不等快照返回：本地读写不一定马上响应取消。恢复失败要报出来，不能当作已经恢复。
func (m *Manager) snapshotWithPause(ctx context.Context, rep *snapshot.Repo, label string, kind snapshot.Kind, dirs []snapshot.Dir, ignore []string, tags map[string]string, stateful bool, onLine func(string)) (string, error) {
	if !stateful {
		return rep.Create(ctx, kind, dirs, ignore, tags)
	}
	resume, err := m.docker.Pause(ctx, label)
	if err != nil {
		return "", fmt.Errorf("暂停容器: %w", err)
	}
	began := time.Now()
	onLine("已暂停容器，正在保存快照")
	sctx, cancel := context.WithTimeout(ctx, pauseLimit)
	defer cancel()
	timer := time.AfterFunc(pauseLimit, func() { _ = resume() })
	sid, err := rep.Create(sctx, kind, dirs, ignore, tags)
	timer.Stop()
	resumeErr := resume()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		// 超时以后才写完的快照，后半段是在容器恢复以后读的，数据可能不一致，不留。
		if sid != "" {
			_ = rep.Delete(context.WithoutCancel(ctx), sid)
		}
		err = fmt.Errorf("暂停达到 %d 秒仍未完成，已放弃本次快照（用时 %s，已写入的内容下次复用）", int(pauseLimit.Seconds()), time.Since(began).Round(time.Second))
		sid = ""
	}
	if resumeErr != nil {
		return "", errors.Join(fmt.Errorf("恢复容器失败，请立即检查容器状态：%w", resumeErr), err)
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
	// 记下时间并清掉失败退避。记不下来时，下一分钟会被当成还没做，所以要报出来。
	if err := m.nodeQ.SetContainerSnapshotTime(ctx, nodestore.SetContainerSnapshotTimeParams{LastSnapshot: time.Now().Unix(), ProjectID: projectID, Name: container}); err != nil {
		return sid, fmt.Errorf("快照已保存，但记录快照时间失败：%w", err)
	}
	onLine("快照已保存")
	return sid, nil
}

// snapshotGap 是容器的定时快照间隔。设置了顶替的容器每 15 分钟一次（与同步给热备的频率一致），除非用户设得更短。
func (m *Manager) snapshotGap(s *nagiv1.ContainerSpec) time.Duration {
	gap := time.Duration(spec.SnapshotIntervalMinutes(s.GetSnapshotIntervalMinutes())) * time.Minute
	if m.FastSnapshot != nil && m.FastSnapshot(s.GetProjectId(), s.GetName()) && gap > failoverSnapshotGap {
		gap = failoverSnapshotGap
	}
	return gap
}

// snapshotDue：到了间隔时间，而且不在失败退避里。
func (m *Manager) snapshotDue(c *Cached, now time.Time) bool {
	return now.Sub(c.LastSnapshot) >= m.snapshotGap(c.Spec) && !now.Before(c.SnapshotRetryAt)
}

// retryDelay 是第 failures 次连续失败以后等多久再试：按 snapshotBackoff 逐级拉长，不超过快照间隔（至少 5 分钟）。
func retryDelay(failures int64, gap time.Duration) time.Duration {
	d := snapshotBackoff[min(int(max(failures, 1))-1, len(snapshotBackoff)-1)]
	return min(d, max(gap, snapshotBackoff[0]))
}

// lowDisk 检查容器目录所在磁盘的剩余空间，不够时返回原因。读不到时不拦。
func lowDisk(dir string) string {
	total, free := metrics.DiskSpace(dir)
	if total <= 0 || free >= minFreeBytes && free*100 >= total*minFreePercent {
		return ""
	}
	return fmt.Sprintf("磁盘剩余空间不足（剩余 %.1f GiB，共 %.1f GiB），已跳过定时快照", float64(free)/(1<<30), float64(total)/(1<<30))
}

// scheduledSnapshot 做一份定时快照。拿到锁以后重新核对是否到期，失败时记下次数和下次重试的时间（存在节点数据库里，重启后照样生效）。
func (m *Manager) scheduledSnapshot(ctx context.Context, projectID, container string) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	now := time.Now()
	if c.Spec.GetVersion() == 0 || !m.snapshotDue(c, now) {
		return errNotDue
	}
	if msg := lowDisk(c.Spec.GetDir()); msg != "" {
		err = errors.New(msg)
	} else {
		_, err = m.takeSnapshot(ctx, projectID, container, snapshot.Scheduled, nil, func(string) {})
	}
	if err == nil {
		return nil
	}
	failures := c.SnapshotFailures + 1
	delay := retryDelay(failures, m.snapshotGap(c.Spec))
	if serr := m.nodeQ.SetContainerSnapshotRetry(ctx, nodestore.SetContainerSnapshotRetryParams{
		SnapshotFailures: failures, SnapshotRetryAt: now.Add(delay).Unix(), ProjectID: projectID, Name: container,
	}); serr != nil {
		err = errors.Join(err, fmt.Errorf("记录重试时间失败：%w", serr))
	}
	return fmt.Errorf("%w（连续第 %d 次失败，%d 分钟后重试）", err, failures, int(delay.Minutes()))
}

// SnapshotDue 由节点每分钟调用一次：给到期的容器做定时快照，再给到期的仓库做维护。两件事各自判断：
// 快照一直失败时维护照样做，失败留下的数据才能回收。task 是 "snapshot" 或 "maintain"。
func (m *Manager) SnapshotDue(ctx context.Context, onResult func(projectID, container, task string, err error)) {
	for _, c := range m.List(ctx) {
		s := c.Spec
		if s.GetVersion() == 0 {
			continue
		}
		if m.snapshotDue(c, time.Now()) {
			if err := m.scheduledSnapshot(ctx, s.GetProjectId(), s.GetName()); !errors.Is(err, errNotDue) && onResult != nil {
				onResult(s.GetProjectId(), s.GetName(), "snapshot", err)
			}
		}
		if time.Since(c.LastMaintain) >= maintainGap {
			err := m.maintain(ctx, s)
			if err != nil {
				// 失败了过一小时再试：把上次维护的时间记成「再过 maintainRetry 就满 24 小时」。
				_ = m.nodeQ.SetContainerMaintenanceTime(ctx, nodestore.SetContainerMaintenanceTimeParams{
					LastMaintenance: time.Now().Add(maintainRetry - maintainGap).Unix(), ProjectID: s.GetProjectId(), Name: s.GetName(),
				})
				err = fmt.Errorf("快照仓库维护: %w", err)
			}
			if onResult != nil {
				onResult(s.GetProjectId(), s.GetName(), "maintain", err)
			}
		}
	}
}

// maintain 运行快照仓库维护，回收过期快照和失败的快照留下的空间。每个容器每天一次，不依赖快照成功。
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

// DeleteSnapshot 删除容器的一份定时、手动或部署前快照。
func (m *Manager) DeleteSnapshot(ctx context.Context, projectID, container, sid string) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	rep, err := m.openRepo(ctx, c.Spec)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)
	if _, err := rep.Tags(ctx, sid); err != nil {
		return invalid("快照不存在：%v", err)
	}
	err = rep.Delete(ctx, sid)
	if errors.Is(err, snapshot.ErrNotDeletable) {
		return invalid("版本文件不能删除，回滚要用")
	}
	return err
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
