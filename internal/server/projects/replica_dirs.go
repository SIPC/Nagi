package projects

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 副本的存放位置（见 docs/snapshot.md#副本的存放位置）：节点有一个默认目录，容器在某个备份节点上的副本可以单独指定。
// 路径由节点自己校验、移动；面板只记下单独指定的位置，用于界面显示。

// SupportsReplicaDirs 判断节点支持选择副本的存放位置（NodeInfo.replica_dirs）。节点不在线时按上次上报的。
func (s *Service) SupportsReplicaDirs(ctx context.Context, nodeID string) bool {
	if conn, err := s.Nodes.Peek(nodeID); err == nil && conn.LastReport() != nil {
		return conn.LastReport().GetNode().GetReplicaDirs()
	}
	n, err := s.Q.GetNode(ctx, nodeID)
	if err != nil {
		return false
	}
	info := &nagiv1.NodeInfo{}
	return proto.Unmarshal(n.Info, info) == nil && info.GetReplicaDirs()
}

// errOldNodeReplicaDir 是节点不支持选择副本存放位置时的错误。
func (s *Service) errOldNodeReplicaDir(ctx context.Context, nodeID string) error {
	return invalid("节点 %s 版本过低，升级后才能选择副本的存放位置", s.NodeName(ctx, nodeID))
}

// nodeMessage 把节点返回的 gRPC 错误换成只有文字的错误（去掉 "rpc error: code = ..."），包进面板的说明里时按语言翻译。
func nodeMessage(err error) error {
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) {
		return errors.New(se.GRPCStatus().Message())
	}
	return err
}

// cleanReplicaDir 整理界面填的存放位置，只做最基本的检查，规则以节点的校验为准。
func cleanReplicaDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !strings.HasPrefix(dir, "/") || slices.Contains(strings.Split(dir, "/"), "..") {
		return "", invalid("存放位置须为绝对路径，且不能包含 ..")
	}
	return path.Clean(dir), nil
}

// checkReplicaDirs 检查设置备份节点时指定的存放位置（节点 ID → 目录），返回整理过的、不为空的那些：
// 只能给新加的备份节点指定，节点要支持。路径规则由节点校验。
func (s *Service) checkReplicaDirs(ctx context.Context, added []string, dirs map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for id, dir := range dirs {
		dir, err := cleanReplicaDir(dir)
		if err != nil {
			return nil, err
		}
		if dir == "" {
			continue
		}
		if !slices.Contains(added, id) {
			return nil, invalid("只能给新加的备份节点指定存放位置，已有的请在备份节点上编辑")
		}
		if !s.SupportsReplicaDirs(ctx, id) {
			return nil, s.errOldNodeReplicaDir(ctx, id)
		}
		out[id] = dir
	}
	return out, nil
}

// backupReplicaDirs 返回容器单独指定了副本存放位置的备份节点（只算现在的备份节点）。
func (s *Service) backupReplicaDirs(ctx context.Context, c store.Container, ids []string) map[string]string {
	if len(ids) == 0 {
		return nil
	}
	all, err := s.Q.ListBackupReplicaDirs(ctx, c.ProjectID, c.Name)
	if err != nil || len(all) == 0 {
		return nil
	}
	out := map[string]string{}
	for id, dir := range all {
		if slices.Contains(ids, id) {
			out[id] = dir
		}
	}
	return out
}

// SetBackupReplicaDir 把容器在备份节点 nodeID 上的副本移到 dir 下面（为空时移回节点的默认目录），把节点的输出转给 out。
// 节点移动完、核对无误才切换，失败时副本留在原处。开始了就做完，浏览器断开也不中断。
func (s *Service) SetBackupReplicaDir(ctx context.Context, projectID, name, nodeID, dir string, out Output) error {
	ctx = context.WithoutCancel(ctx)
	dir, err := cleanReplicaDir(dir)
	if err != nil {
		return err
	}
	c, err := s.container(ctx, projectID, name)
	if err != nil {
		return err
	}
	if !slices.Contains(store.BackupNodes(c), nodeID) {
		return invalid("节点 %s 不是容器 %s 的备份节点", s.NodeName(ctx, nodeID), name)
	}
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return s.nodeErr(ctx, nodeID, err)
	}
	if !conn.LastReport().GetNode().GetReplicaDirs() {
		return s.errOldNodeReplicaDir(ctx, nodeID)
	}
	// 和节点的默认目录相同时按默认处理（节点也是这样）。
	if dir != "" && dir == conn.LastReport().GetNode().GetReplicaDir() {
		dir = ""
	}
	stream, err := conn.Agent.MoveReplica(ctx, &nagiv1.MoveReplicaRequest{ProjectId: projectID, Container: name, Dir: dir})
	if err != nil {
		return s.nodeErr(ctx, nodeID, err)
	}
	if _, err := recvResult(stream.Recv, out); err != nil {
		return err
	}
	if err := s.Q.SetBackupReplicaDir(ctx, projectID, name, nodeID, dir); err != nil {
		return i18n.Errorf("副本已移动，但面板未能记下新位置：%w", err)
	}
	s.Log.Info("已修改副本的存放位置", "project", projectID, "container", name, "node", nodeID, "dir", dir)
	return nil
}
