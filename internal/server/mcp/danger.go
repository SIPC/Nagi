package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 不可逆或会中断服务的操作（见 docs/mcp.md#工具）：删除、回到某次部署、迁移、备份节点和顶替。
// 都分两步：不带 confirm 只返回预览（会发生什么、会丢什么）；confirm 原样填工具说明里写的值才执行。

// preview 是第一步的返回：给用户看的说明，和第二步怎么调用。
func preview(tool, confirm, text string, extra map[string]any) map[string]any {
	out := map[string]any{"preview": text,
		"next": fmt.Sprintf("show this to the user; after they agree, call %s again with confirm set to %q", tool, confirm)}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---- 删除 ----

type DeleteContainerArgs struct {
	ContainerArg
	DeleteDir        bool   `json:"delete_dir,omitempty" jsonschema:"also delete the container directory on the node: its data, volumes and snapshots are gone for good. Default false keeps the directory"`
	ExpectedRevision int64  `json:"expected_revision,omitempty" jsonschema:"the container's revision from get_container"`
	Confirm          string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the container name here to delete after the user agreed"`
}

func toolDeleteContainer(c *call, in DeleteContainerArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	if in.Confirm != m.GetName() {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		routes := []string{}
		for _, r := range p.GetRoutes() {
			if r.GetContainer() == m.GetName() {
				routes = append(routes, r.GetName())
			}
		}
		users := []string{}
		for _, o := range p.GetContainers() {
			if slices.Contains(o.GetConfig().GetDependsOn(), m.GetName()) {
				users = append(users, o.GetName())
			}
		}
		text := fmt.Sprintf("container %s on node %s will be stopped and removed with its versions; its port mappings stop working.", m.GetName(), nodeName(nodes, m.GetNodeId()))
		if in.DeleteDir {
			text += " Its directory " + m.GetDir() + " is deleted too: data, volumes and snapshots cannot be recovered."
		} else {
			text += " Its directory " + m.GetDir() + " stays on the node with the data and snapshots."
		}
		return preview("delete_container", m.GetName(), text, map[string]any{"port_mappings": routes, "depended_on_by": users}), nil
	}
	if _, err := c.h.clients.projects.DeleteContainer(c.ctx, connect.NewRequest(&nagiv1.DeleteContainerApiRequest{
		ProjectId: p.GetId(), Container: m.GetName(), BaseRevision: baseRevision(in.ExpectedRevision, m), DeleteDir: in.DeleteDir,
	})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "container deleted"}, nil
}

type DeleteProjectArgs struct {
	ProjectArg
	DeleteDirs       bool   `json:"delete_dirs,omitempty" jsonschema:"also delete every container directory: data, volumes and snapshots are gone for good. Default false keeps them"`
	ExpectedRevision int64  `json:"expected_revision,omitempty" jsonschema:"the project's revision from get_project"`
	Confirm          string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the project name here to delete after the user agreed"`
}

func toolDeleteProject(c *call, in DeleteProjectArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	if in.Confirm != p.GetName() {
		names := []string{}
		for _, m := range p.GetContainers() {
			names = append(names, m.GetName())
		}
		text := fmt.Sprintf("project %s will be deleted: all %d containers are stopped and removed, with their port mappings, versions, deployment history and links to other projects.", p.GetName(), len(names))
		if in.DeleteDirs {
			text += " The container directories are deleted too: data, volumes and snapshots cannot be recovered."
		} else {
			text += " The container directories stay on the nodes with the data and snapshots."
		}
		return preview("delete_project", p.GetName(), text, map[string]any{"containers": names, "port_mappings": len(p.GetRoutes()), "linked_projects": len(p.GetLinks())}), nil
	}
	if _, err := c.h.clients.projects.DeleteProject(c.ctx, connect.NewRequest(&nagiv1.DeleteProjectRequest{
		ProjectId: p.GetId(), BaseRevision: projectRevision(in.ExpectedRevision, p), DeleteDir: in.DeleteDirs, ConfirmName: p.GetName(),
	})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "project deleted"}, nil
}

type DeleteFileArgs struct {
	FileArg
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the path here to delete after the user agreed"`
}

func toolDeleteFile(c *call, in DeleteFileArgs) (any, error) {
	ref, err := c.fileRef(in.FileArg)
	if err != nil {
		return nil, err
	}
	if in.Confirm != in.Path {
		return preview("delete_file", in.Path, fmt.Sprintf("%s in volume %s will be deleted now; a directory is deleted with everything in it. It can only be brought back from a snapshot (restore_file).", ref.GetPath(), ref.GetVolume()), nil), nil
	}
	if _, err := c.h.clients.projects.DeleteVolumeFile(c.ctx, connect.NewRequest(ref)); err != nil {
		return nil, err
	}
	return map[string]string{"note": "deleted"}, nil
}

// ---- 回到某次部署、回滚时恢复数据 ----

type RestoreDeploymentArgs struct {
	ProjectArg
	DeploymentID int64  `json:"deployment_id" jsonschema:"deployment to go back to (see list_deployments)"`
	Confirm      string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the deployment_id here (as text) to run it after the user agreed"`
}

func toolRestoreDeployment(c *call, in RestoreDeploymentArgs) (any, error) {
	s, err := c.project(in.Project)
	if err != nil {
		return nil, err
	}
	p := s.GetProject()
	id := strconv.FormatInt(in.DeploymentID, 10)
	if in.Confirm != id {
		res, err := c.h.clients.projects.ListDeployments(c.ctx, connect.NewRequest(&nagiv1.ProjectRef{ProjectId: p.GetId()}))
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(res.Msg.GetDeployments(), func(d *nagiv1.Deployment) bool { return d.GetId() == in.DeploymentID })
		if i < 0 {
			return nil, fmt.Errorf("deployment %d not found (see list_deployments)", in.DeploymentID)
		}
		d := res.Msg.GetDeployments()[i]
		var changes []string
		for _, m := range p.GetContainers() {
			v, ok := d.GetVersions()[m.GetName()]
			if ok && v != m.GetDeployedVersion() {
				changes = append(changes, fmt.Sprintf("%s: version %d → %d", m.GetName(), m.GetDeployedVersion(), v))
			}
		}
		if len(changes) == 0 {
			return nil, errors.New("every container is already at the version of that deployment")
		}
		return preview("restore_deployment", id, fmt.Sprintf("the containers below are rolled back to their versions at deployment %d (%s), deployed together. Configuration, files and images go back; data is not restored. Containers added later are not touched.", d.GetId(), ts(d.GetTime())),
			map[string]any{"changes": changes}), nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.RestoreDeployment(ctx, connect.NewRequest(&nagiv1.RestoreDeploymentRequest{ProjectId: p.GetId(), DeploymentId: in.DeploymentID}))
	})
}

// ---- 备份节点 ----

type BackupNodeArgs struct {
	ContainerArg
	Node          string `json:"node,omitempty" jsonschema:"backup node name or ID; empty removes the backup node"`
	DeleteReplica bool   `json:"delete_replica,omitempty" jsonschema:"when removing: also delete the copy on the old backup node"`
	Confirm       string `json:"confirm,omitempty" jsonschema:"only when removing: omit to get a preview, then repeat the container name here"`
}

func toolSetBackupNode(c *call, in BackupNodeArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	req := &nagiv1.SetBackupNodeRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName()}
	if in.Node != "" {
		n, err := findNode(nodes, in.Node)
		if err != nil {
			return nil, err
		}
		req.NodeId = n.GetId()
	} else {
		if m.GetBackupNodeId() == "" {
			return map[string]string{"note": "the container has no backup node"}, nil
		}
		if in.Confirm != m.GetName() {
			text := fmt.Sprintf("container %s stops syncing to backup node %s; if its node fails, there is no copy to restore from.", m.GetName(), nodeName(nodes, m.GetBackupNodeId()))
			if in.DeleteReplica {
				text += " The copy on the backup node is deleted."
			} else {
				text += " The copy on the backup node stays until deleted."
			}
			return preview("set_backup_node", m.GetName(), text, nil), nil
		}
		req.DeleteReplica = in.DeleteReplica
	}
	if _, err := c.h.clients.projects.SetBackupNode(c.ctx, connect.NewRequest(req)); err != nil {
		return nil, err
	}
	if req.NodeId == "" {
		return map[string]string{"note": "backup node removed"}, nil
	}
	return map[string]string{"note": "backup node set; snapshots sync after each scheduled snapshot, or now with sync_backup"}, nil
}

type RestoreFromBackupArgs struct {
	ContainerArg
	SnapshotID string `json:"snapshot_id,omitempty" jsonschema:"snapshot in the backup copy (see list_backup_snapshots); default the newest scheduled or manual one"`
	Dir        string `json:"dir,omitempty" jsonschema:"container directory on the backup node; default from its projects root"`
	Confirm    string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the container name here to restore after the user agreed"`
}

func toolRestoreFromBackup(c *call, in RestoreFromBackupArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if m.GetBackupNodeId() == "" {
		return nil, errors.New("the container has no backup node")
	}
	if in.Confirm != m.GetName() {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		// 原节点离线时它上报的同步状态也没了，直接看备份节点上的副本里有哪些快照。
		res, err := c.h.clients.projects.ListBackupSnapshots(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()}))
		if err != nil {
			return nil, err
		}
		var sn *nagiv1.Snapshot
		for _, x := range res.Msg.GetSnapshots() {
			if in.SnapshotID == "" && restorable(x.GetKind()) || x.GetId() == in.SnapshotID {
				sn = x
				break
			}
		}
		from := "the version files of the deployed version (the copy has no data snapshot yet, so it starts without data)"
		if sn != nil {
			from = fmt.Sprintf("the %s snapshot of %s", sn.GetKind(), ts(sn.GetTime()))
		} else if in.SnapshotID != "" {
			return nil, fmt.Errorf("snapshot %q is not in the backup copy (see list_backup_snapshots)", in.SnapshotID)
		}
		return preview("restore_from_backup", m.GetName(), fmt.Sprintf("only for when node %s is offline: container %s is started on backup node %s from %s in its copy. Data written after that is lost. The container then lives on %s; when %s comes back it stops its old copy and keeps the directory renamed.",
			nodeName(nodes, m.GetNodeId()), m.GetName(), nodeName(nodes, m.GetBackupNodeId()), from, nodeName(nodes, m.GetBackupNodeId()), nodeName(nodes, m.GetNodeId())), nil), nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.RestoreFromBackup(ctx, connect.NewRequest(&nagiv1.RestoreFromBackupRequest{
			ProjectId: s.GetProject().GetId(), Container: m.GetName(), SnapshotId: in.SnapshotID, Dir: in.Dir,
		}))
	})
}

// ---- 迁移、换节点重新部署 ----

type MoveArgs struct {
	ContainerArg
	Node    string `json:"node" jsonschema:"target node name or ID"`
	Dir     string `json:"dir,omitempty" jsonschema:"container directory on the target node; default from its projects root"`
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the target node name here after the user agreed"`
}

func (c *call) moveTarget(in MoveArgs) (*nagiv1.ProjectSummary, *nagiv1.Container, *nagiv1.ListNodesResponse, *nagiv1.Node, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if n.GetId() == m.GetNodeId() {
		return nil, nil, nil, nil, errors.New("the container is already on that node")
	}
	return s, m, nodes, n, nil
}

func toolMigrateContainer(c *call, in MoveArgs) (any, error) {
	s, m, nodes, n, err := c.moveTarget(in)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(in.Confirm, n.GetName()) && in.Confirm != n.GetId() {
		return preview("migrate_container", n.GetName(), fmt.Sprintf("container %s moves from %s to %s without losing data: it keeps running for a first sync, then stops for the final sync and starts on %s. The directory on %s is kept for you to delete. Both nodes must be online; a container with failover must have it removed first, and a backup node must be chosen again afterwards. Any failure rolls back and restarts it on %s.",
			m.GetName(), nodeName(nodes, m.GetNodeId()), n.GetName(), n.GetName(), nodeName(nodes, m.GetNodeId()), nodeName(nodes, m.GetNodeId())), nil), nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.MigrateContainer(ctx, connect.NewRequest(&nagiv1.MoveContainerRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), NodeId: n.GetId(), Dir: in.Dir}))
	})
}

func toolRedeployElsewhere(c *call, in MoveArgs) (any, error) {
	s, m, nodes, n, err := c.moveTarget(in)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(in.Confirm, n.GetName()) && in.Confirm != n.GetId() {
		return preview("redeploy_elsewhere", n.GetName(), fmt.Sprintf("last resort when node %s is offline and there is no copy to restore from: container %s is deployed fresh on %s from its latest configuration. Its data and the files of old versions stay on %s, so it starts with empty data and cannot roll back to older versions. When %s comes back it stops its old copy and keeps the directory renamed. If the node is online, use migrate_container; with a backup node, use restore_from_backup.",
			nodeName(nodes, m.GetNodeId()), m.GetName(), n.GetName(), nodeName(nodes, m.GetNodeId()), nodeName(nodes, m.GetNodeId())), nil), nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.RedeployElsewhere(ctx, connect.NewRequest(&nagiv1.MoveContainerRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), NodeId: n.GetId(), Dir: in.Dir}))
	})
}

// ---- 顶替 ----

type FailoverArgs struct {
	ContainerArg
	StandbyNode  string `json:"standby_node" jsonschema:"node that takes over when the container's node fails (name or ID)"`
	StandbyDir   string `json:"standby_dir,omitempty" jsonschema:"container directory on the standby node; default from its projects root"`
	WitnessNode  string `json:"witness_node,omitempty" jsonschema:"third node that only votes; default the panel"`
	LeaseSeconds uint32 `json:"lease_seconds,omitempty" jsonschema:"lease length; default 30"`
	AutoStateful bool   `json:"auto_stateful,omitempty" jsonschema:"also take over stateful containers automatically, losing data written after the last sync; default false waits for confirm_takeover"`
	AutoFailback bool   `json:"auto_failback,omitempty" jsonschema:"move the container back automatically once its node is stable again (briefly stopped, no data lost)"`
	Confirm      string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the standby node name here after the user agreed"`
}

func toolSetFailover(c *call, in FailoverArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	b, err := findNode(nodes, in.StandbyNode)
	if err != nil {
		return nil, err
	}
	req := &nagiv1.SetFailoverApiRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), StandbyNodeId: b.GetId(), StandbyDir: in.StandbyDir,
		LeaseSeconds: in.LeaseSeconds, AutoStateful: in.AutoStateful, AutoFailback: in.AutoFailback}
	witness := "the panel"
	if in.WitnessNode != "" {
		w, err := findNode(nodes, in.WitnessNode)
		if err != nil {
			return nil, err
		}
		req.WitnessNodeId, witness = w.GetId(), w.GetName()
	}
	if !strings.EqualFold(in.Confirm, b.GetName()) && in.Confirm != b.GetId() {
		text := fmt.Sprintf("%s keeps a synced copy of container %s and takes over if %s fails, decided by a lease and votes from the two nodes and %s. Snapshots run every 15 minutes or more often and sync to %s.",
			b.GetName(), m.GetName(), nodeName(nodes, m.GetNodeId()), witness, b.GetName())
		if m.GetStateful() && !in.AutoStateful {
			text += " The container is stateful: a takeover waits for confirm_takeover."
		} else if m.GetStateful() {
			text += " The container is stateful and takes over automatically: data written after the last sync is lost."
		}
		if m.GetBackupNodeId() != "" {
			text += " The container has a backup node, which must be removed first."
		}
		return preview("set_failover", b.GetName(), text, nil), nil
	}
	if _, err := c.h.clients.projects.SetFailover(c.ctx, connect.NewRequest(req)); err != nil {
		return nil, err
	}
	return map[string]string{"note": "failover set"}, nil
}

type ConfirmArgs struct {
	ContainerArg
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; repeat the container name here after the user agreed"`
}

func toolClearFailover(c *call, in ConfirmArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if m.GetFailover() == nil {
		return map[string]string{"note": "the container has no failover"}, nil
	}
	if in.Confirm != m.GetName() {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		return preview("clear_failover", m.GetName(), fmt.Sprintf("container %s stays on the node running it now and no longer has a standby on %s; if that node fails, nothing takes over.",
			m.GetName(), nodeName(nodes, m.GetFailover().GetB().GetNodeId())), nil), nil
	}
	if _, err := c.h.clients.projects.ClearFailover(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "failover removed"}, nil
}

type WitnessArgs struct {
	ContainerArg
	WitnessNode string `json:"witness_node,omitempty" jsonschema:"node name or ID; empty makes the panel the witness"`
}

func toolChangeWitness(c *call, in WitnessArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if m.GetFailover() == nil {
		return nil, errors.New("the container has no failover")
	}
	req := &nagiv1.ChangeWitnessRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName()}
	if in.WitnessNode != "" {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		w, err := findNode(nodes, in.WitnessNode)
		if err != nil {
			return nil, err
		}
		req.WitnessNodeId = w.GetId()
	}
	if _, err := c.h.clients.projects.ChangeWitness(c.ctx, connect.NewRequest(req)); err != nil {
		return nil, err
	}
	return map[string]string{"note": "witness changed"}, nil
}

func toolConfirmTakeover(c *call, in ConfirmArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	st := m.GetFailoverStandby()
	if m.GetFailover() == nil || !st.GetPendingConfirm() {
		return nil, errors.New("no takeover is waiting for confirmation (the standby has not lost the container's node)")
	}
	if in.Confirm != m.GetName() {
		nodes, err := c.nodes()
		if err != nil {
			return nil, err
		}
		return preview("confirm_takeover", m.GetName(), fmt.Sprintf("the standby node starts container %s from the last data synced to it (%s; before the first data snapshot only the deployed version's files, without data). Data written after that on the lost node %s is lost. Do this only if that node is really down; it is stopped when it comes back.",
			m.GetName(), ts(st.GetLastSync()), nodeName(nodes, m.GetNodeId())), nil), nil
	}
	if _, err := c.h.clients.projects.ConfirmTakeover(c.ctx, connect.NewRequest(&nagiv1.ContainerRef{ProjectId: s.GetProject().GetId(), Container: m.GetName()})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "takeover confirmed; the standby is starting the container"}, nil
}
