package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 快照：列出、恢复和删除（见 docs/mcp.md#快照）。恢复会停止容器、替换它的全部数据，删除没法撤销，所以都分两步：
// 不带确认时只返回预览；确认时原样带上快照 ID。恢复前先给当前数据拍一份保护快照，拍成功了才恢复。

type snapshotView struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Time       string `json:"time"`
	Bytes      int64  `json:"size_bytes"`
	Version    int64  `json:"version,omitempty"`
	Restorable bool   `json:"restorable"`
	Deletable  bool   `json:"deletable"`
}

// restorable：只能从定时快照和手动快照恢复数据（见 docs/snapshot.md）。部署前的快照随回滚使用，版本文件随版本回滚。
func restorable(kind string) bool { return kind == "scheduled" || kind == "manual" }

// deletable：版本文件不能删，回滚要用。定时、手动、部署前快照也计入项目设置里的份数上限。
func deletable(kind string) bool { return restorable(kind) || kind == "predeploy" }

func view(sn *nagiv1.Snapshot) snapshotView {
	return snapshotView{ID: sn.GetId(), Kind: sn.GetKind(), Time: ts(sn.GetTime()), Bytes: sn.GetSize(), Version: sn.GetVersion(),
		Restorable: restorable(sn.GetKind()), Deletable: deletable(sn.GetKind())}
}

// pushedOut 判断再拍一份快照后，sn 会不会因为份数上限被删掉。list 是最新的在前。
func pushedOut(list []*nagiv1.Snapshot, sn *nagiv1.Snapshot, limit uint32) bool {
	n := 0
	for _, x := range list {
		if !deletable(x.GetKind()) {
			continue
		}
		if x.GetId() == sn.GetId() {
			return uint32(n+1) >= limit
		}
		n++
	}
	return false
}

// ---- 备份节点 ----

func toolSyncBackup(c *call, in ContainerArg) (any, error) {
	s, m, err := c.container(in)
	if err != nil {
		return nil, err
	}
	if m.GetBackupNodeId() == "" {
		return nil, errors.New("the container has no backup node")
	}
	start := time.Now().Add(-time.Second)
	if _, err := c.h.clients.projects.SyncBackup(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()})); err != nil {
		return nil, err
	}
	// 同步状态由节点上报，同步完以后过一会儿才到面板上。
	var b *nagiv1.BackupStatus
	for i := 0; i < 10; i++ {
		p, err := c.getProject(s.GetProject().GetId())
		if err != nil {
			return nil, err
		}
		if after, err := findContainer(p.GetProject(), m.GetName()); err == nil {
			b = after.GetBackup()
		}
		if b.GetLastError() != "" || b.GetLastSync().AsTime().After(start) {
			break
		}
		time.Sleep(time.Second)
	}
	if b.GetLastError() == "" && !b.GetLastSync().AsTime().After(start) {
		return map[string]any{"note": "sync finished; the node has not reported the new state yet, check get_container in a moment"}, nil
	}
	return map[string]any{"backup_node": b.GetTargetName(), "last_sync": ts(b.GetLastSync()), "error": b.GetLastError()}, nil
}

func toolListBackupSnapshots(c *call, in ContainerArg) (any, error) {
	s, m, err := c.container(in)
	if err != nil {
		return nil, err
	}
	if m.GetBackupNodeId() == "" {
		return nil, errors.New("the container has no backup node")
	}
	res, err := c.h.clients.projects.ListBackupSnapshots(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}))
	if err != nil {
		return nil, err
	}
	out := []snapshotView{}
	for _, sn := range res.Msg.GetSnapshots() {
		v := view(sn)
		v.Restorable, v.Deletable = false, false
		out = append(out, v)
	}
	return map[string]any{"snapshots": out, "note": "snapshots in the copy on the backup node, as of its last sync. If the container's node is offline, restore_from_backup starts it from one of them"}, nil
}

func (c *call) snapshots(ref *nagiv1.ContainerRef) ([]*nagiv1.Snapshot, error) {
	res, err := c.h.clients.projects.ListSnapshots(c.ctx, connect.NewRequest(ref))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetSnapshots(), nil
}

func toolListSnapshots(c *call, in ContainerArg) (any, error) {
	s, m, err := c.container(in)
	if err != nil {
		return nil, err
	}
	list, err := c.snapshots(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()})
	if err != nil {
		return nil, err
	}
	out := []snapshotView{}
	for _, sn := range list {
		out = append(out, view(sn))
	}
	res := map[string]any{"snapshots": out, "note": "only scheduled and manual snapshots can be restored with restore_snapshot; predeploy and version snapshots are used by rollback. version snapshots cannot be deleted"}
	res["limit"] = spec.SnapshotLimit(s.GetProject().GetSettings().GetSnapshotLimit())
	res["limit_note"] = "scheduled, manual and predeploy snapshots together are kept up to this count; the oldest are deleted after each new snapshot"
	return res, nil
}

type RestoreArgs struct {
	ContainerArg
	SnapshotID string `json:"snapshot_id" jsonschema:"snapshot to restore (see list_snapshots)"`
	Confirm    string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat snapshot_id here to restore after the user agreed to the preview"`
}

func toolRestoreSnapshot(c *call, in RestoreArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	ref := &nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}
	list, err := c.snapshots(ref)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(x *nagiv1.Snapshot) bool { return x.GetId() == in.SnapshotID })
	if i < 0 {
		return nil, fmt.Errorf("snapshot %q not found for container %s", in.SnapshotID, m.GetName())
	}
	sn := list[i]
	if !restorable(sn.GetKind()) {
		return nil, fmt.Errorf("a %s snapshot cannot be restored here; use rollback for version and predeploy data", sn.GetKind())
	}
	// 保护快照会让份数超过上限时，最老的会被删掉；要恢复的正好是它就不做，免得把它删了。
	if pushedOut(list, sn, spec.SnapshotLimit(s.GetProject().GetSettings().GetSnapshotLimit())) {
		return nil, errors.New("this snapshot would be deleted by the project's snapshot limit when the protective snapshot is taken; ask the user to raise the limit in project settings or delete newer snapshots first")
	}
	if in.Confirm != in.SnapshotID {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"preview":  true,
			"snapshot": snapshotView{ID: sn.GetId(), Kind: sn.GetKind(), Time: ts(sn.GetTime()), Bytes: sn.GetSize(), Restorable: true},
			"container": map[string]any{
				"name": m.GetName(), "node": nodeName(nodes, m.GetNodeId()), "deployed_version": m.GetDeployedVersion(), "stateful": m.GetStateful(),
			},
			"effects": []string{
				"a manual snapshot of the current data is taken first; the restore does not start if it fails",
				"the container is stopped, its data/ and volumes/ are replaced with the snapshot, then it is started again",
				"data written after the snapshot time is lost from the running container (it stays in the protective snapshot)",
				"configuration, version and port mappings do not change",
			},
			"next": "show this preview to the user; after they agree, call restore_snapshot again with confirm set to the snapshot_id",
		}, nil
	}
	before := map[string]bool{}
	for _, x := range list {
		before[x.GetId()] = true
	}
	protect, err := c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.CreateSnapshot(ctx, connect.NewRequest(ref))
	})
	if err != nil {
		return nil, err
	}
	if !protect.Success {
		return nil, fmt.Errorf("the protective snapshot failed, so nothing was restored: %s", protect.Error)
	}
	protectID := ""
	if after, err := c.snapshots(ref); err == nil {
		for _, x := range after {
			if !before[x.GetId()] && x.GetKind() == "manual" {
				protectID = x.GetId()
			}
		}
	}
	if protectID == "" {
		return nil, errors.New("the protective snapshot did not show up in the snapshot list, so nothing was restored")
	}
	res, err := c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.RestoreSnapshot(ctx, connect.NewRequest(&nagiv1.RestoreSnapshotRequest{
			ProjectId: ref.GetProjectId(), Container: ref.GetContainer(), SnapshotId: sn.GetId(),
		}))
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"protective_snapshot": protectID, "restore": res}, nil
}

type DeleteSnapshotArgs struct {
	ContainerArg
	SnapshotID string `json:"snapshot_id" jsonschema:"snapshot to delete (see list_snapshots)"`
	Confirm    string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat snapshot_id here to delete after the user agreed"`
}

func toolDeleteSnapshot(c *call, in DeleteSnapshotArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	ref := &nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}
	list, err := c.snapshots(ref)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(x *nagiv1.Snapshot) bool { return x.GetId() == in.SnapshotID })
	if i < 0 {
		return nil, fmt.Errorf("snapshot %q not found for container %s", in.SnapshotID, m.GetName())
	}
	sn := list[i]
	if !deletable(sn.GetKind()) {
		return nil, errors.New("version snapshots cannot be deleted; rollback needs them")
	}
	if in.Confirm != in.SnapshotID {
		return map[string]any{
			"preview":   true,
			"snapshot":  view(sn),
			"container": m.GetName(),
			"effects": []string{
				"the snapshot is deleted permanently and cannot be restored from afterwards",
				"the container and its current data are not touched",
				"disk space is freed by the daily maintenance, a day or two later",
			},
			"next": "show this preview to the user; after they agree, call delete_snapshot again with confirm set to the snapshot_id",
		}, nil
	}
	res, err := c.h.clients.projects.DeleteSnapshot(c.ctx, connect.NewRequest(&nagiv1.RestoreSnapshotRequest{
		ProjectId: ref.GetProjectId(), Container: ref.GetContainer(), SnapshotId: sn.GetId(),
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": sn.GetId(), "remaining": len(res.Msg.GetSnapshots())}, nil
}
