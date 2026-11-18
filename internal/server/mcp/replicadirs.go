package mcp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 副本的存放位置（见 docs/snapshot.md#副本的存放位置、docs/mcp.md#副本的存放位置）：修改节点的副本目录、
// 修改容器在一个备份节点上的副本存放位置。这等于让节点以 root 在任意位置建目录、写数据，所以和 node_exec 一样：
// 只给管理员、令牌勾了「可修改」和「节点终端」；每次都先预览，用户同意后带上按工具、节点和参数算出的确认码才执行。
// 令牌做不了高权限确认，接口只对这两个接口、这样的令牌放行（api.tokenElevation）。

func init() {
	allTools = append(allTools, replicaDirTools...)
}

var replicaDirTools = []tool{
	def(tool{name: "set_node_replica_dir", title: "Set node replica directory", write: true, destructive: true, nodeExec: true,
		description: "Administrators only, and only with a token created with the allow-changes and node terminal permissions. Change where a node stores the replicas it receives as a backup node or failover standby (dir empty means the default <node data dir>/replicas). The node validates the path and moves the replicas in the old default directory there, verifying every file before switching; replicas with their own location do not move, and a replica whose move fails stays where it was. Syncing of a replica pauses while it moves. Every call previews first: show the preview to the user, and only after they agree call again with the same arguments and confirm set to the code from the preview.",
		procedures:  []string{nagiv1connect.NodeServiceListNodesProcedure, nagiv1connect.NodeServiceListNodeReplicasProcedure, nagiv1connect.NodeServiceSetNodeReplicaDirProcedure}},
		toolSetNodeReplicaDir),
	def(tool{name: "set_replica_location", title: "Set replica location", write: true, destructive: true, nodeExec: true,
		description: "Administrators only, and only with a token created with the allow-changes and node terminal permissions. Change where one container's replica is stored on one of its backup nodes (dir empty moves it back to the node's replica directory). The node validates the path, moves the replica there and verifies it before switching; on failure it stays where it was. Syncing to that node pauses during the move. Every call previews first: show the preview to the user, and only after they agree call again with the same arguments and confirm set to the code from the preview.",
		procedures: []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure, nagiv1connect.NodeServiceListNodesProcedure,
			nagiv1connect.ProjectServiceSetBackupReplicaDirProcedure}},
		toolSetReplicaLocation),
}

// replicaDirParts 是确认码的内容（确认码见 confirm.go）：工具、节点、容器和整理过的目录，预览之后改了其中任何一项都对不上。
func replicaDirParts(tool, nodeID, projectID, container, dir string) []string {
	return []string{tool, nodeID, projectID, container, dir}
}

// cleanDir 整理参数里的目录：空的表示默认目录；其余要是绝对路径、不含 ..（节点还会按自己的规则校验）。
func cleanDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !strings.HasPrefix(dir, "/") || slices.Contains(strings.Split(dir, "/"), "..") {
		return "", errors.New("dir must be an absolute path without ..")
	}
	return path.Clean(dir), nil
}

const defaultReplicaDir = "<node data dir>/replicas (default)"

type SetNodeReplicaDirArgs struct {
	Node    string `json:"node" jsonschema:"node name or ID (see list_nodes)"`
	Dir     string `json:"dir,omitempty" jsonschema:"absolute path for the node's replicas, for example a mounted data disk; empty means the default <node data dir>/replicas"`
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; after the user agreed, call again with the same node and dir and confirm set to the code from the preview"`
}

func toolSetNodeReplicaDir(c *call, in SetNodeReplicaDirArgs) (any, error) {
	dir, err := cleanDir(in.Dir)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	if !n.GetInfo().GetReplicaDirs() {
		return nil, fmt.Errorf("node %s is out of date; upgrade it to choose where replicas are stored", n.GetName())
	}
	if code, ok := c.confirm(in.Confirm, replicaDirParts("set_node_replica_dir", n.GetId(), "", "", dir)...); !ok {
		var moving []string
		if res, err := c.h.clients.nodes.ListNodeReplicas(c.ctx, connect.NewRequest(&nagiv1.NodeRef{NodeId: n.GetId()})); err == nil {
			for _, r := range res.Msg.GetReplicas() {
				if !r.GetReplica().GetCustomDir() {
					moving = append(moving, r.GetReplica().GetProjectName()+" / "+r.GetReplica().GetContainer())
				}
			}
		}
		out := preview("set_node_replica_dir", code, fmt.Sprintf("node %s will store replicas in this directory and move the replicas below there. The node writes there as root. Nothing changes until the user agrees.", n.GetName()),
			map[string]any{"node": n.GetName(), "current": orDefault(n.GetInfo().GetReplicaDir()), "new": orDefault(dir), "replicas_to_move": moving})
		if in.Confirm != "" {
			out["note"] = "the confirm code does not match this node and dir; show this preview to the user again"
		}
		return out, nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.nodes.SetNodeReplicaDir(ctx, connect.NewRequest(&nagiv1.SetNodeReplicaDirRequest{NodeId: n.GetId(), Dir: dir}))
	})
}

func orDefault(dir string) string {
	if dir == "" {
		return defaultReplicaDir
	}
	return dir
}

type SetReplicaLocationArgs struct {
	ContainerArg
	Node    string `json:"node" jsonschema:"one of the container's backup nodes (name or ID)"`
	Dir     string `json:"dir,omitempty" jsonschema:"absolute path on that node; the replica goes to <dir>/<project ID>-<container>. Empty moves it back to the node's replica directory"`
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; after the user agreed, call again with the same arguments and confirm set to the code from the preview"`
}

func toolSetReplicaLocation(c *call, in SetReplicaLocationArgs) (any, error) {
	dir, err := cleanDir(in.Dir)
	if err != nil {
		return nil, err
	}
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes()
	if err != nil {
		return nil, err
	}
	n, err := findNode(nodes, in.Node)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(m.GetBackupNodeIds(), n.GetId()) {
		return nil, fmt.Errorf("node %s is not a backup node of container %s; add it with set_backup_node first", n.GetName(), m.GetName())
	}
	if !n.GetInfo().GetReplicaDirs() {
		return nil, fmt.Errorf("node %s is out of date; upgrade it to choose where replicas are stored", n.GetName())
	}
	p := s.GetProject()
	if code, ok := c.confirm(in.Confirm, replicaDirParts("set_replica_location", n.GetId(), p.GetId(), m.GetName(), dir)...); !ok {
		current := m.GetBackupReplicaDirs()[n.GetId()]
		if current == "" {
			current = "the node's replica directory"
		}
		target := dir
		if target == "" {
			target = "the node's replica directory"
		}
		out := preview("set_replica_location", code, fmt.Sprintf("the replica of %s / %s on node %s moves to this location. The node writes there as root. Nothing changes until the user agrees.", p.GetName(), m.GetName(), n.GetName()),
			map[string]any{"container": p.GetName() + " / " + m.GetName(), "node": n.GetName(), "current": current, "new": target})
		if in.Confirm != "" {
			out["note"] = "the confirm code does not match these arguments; show this preview to the user again"
		}
		return out, nil
	}
	return c.follow(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.DeployEvent], error) {
		return c.h.clients.projects.SetBackupReplicaDir(ctx, connect.NewRequest(&nagiv1.SetBackupReplicaDirRequest{ProjectId: p.GetId(), Container: m.GetName(), NodeId: n.GetId(), Dir: dir}))
	})
}
