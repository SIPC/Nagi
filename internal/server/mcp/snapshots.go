package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 快照：列出和恢复（见 docs/mcp.md#快照）。恢复会停止容器、替换它的全部数据，所以分两步：
// 不带确认时只返回预览；确认时原样带上快照 ID，先给当前数据拍一份保护快照，拍成功了才恢复。

type snapshotView struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Time       string `json:"time"`
	Bytes      int64  `json:"size_bytes"`
	Version    int64  `json:"version,omitempty"`
	Restorable bool   `json:"restorable"`
}

// restorable：只能从定时快照和手动快照恢复数据（见 docs/snapshot.md）。部署前的快照随回滚使用，版本文件随版本回滚。
func restorable(kind string) bool { return kind == "scheduled" || kind == "manual" }

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
		out = append(out, snapshotView{ID: sn.GetId(), Kind: sn.GetKind(), Time: ts(sn.GetTime()), Bytes: sn.GetSize(), Version: sn.GetVersion(), Restorable: restorable(sn.GetKind())})
	}
	return map[string]any{"snapshots": out, "note": "only scheduled and manual snapshots can be restored with restore_snapshot; predeploy and version snapshots are used by rollback"}, nil
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
