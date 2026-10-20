package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 备份节点和迁移（见 docs/snapshot.md#备份节点的实现、docs/failover.md#手动迁移），按容器进行。
//   - 源节点：每次定时快照成功后，把容器的快照仓库经 mesh 增量同步到备份节点（备份节点在面板下发的 ContainerSpec.backup 里）；
//   - 目标节点：副本放在 <节点目录>/replicas/<项目 ID>-<容器名>/files/repo，只接受登记过的源节点同步；
//     恢复用的配置是面板登记副本时给的（spec.pb），不从源节点拿。

var projectIDRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

type replicaMeta struct {
	ProjectID    string `json:"project_id"`
	Container    string `json:"container"`
	SourceKey    []byte `json:"source_key"`
	SourceNodeID string `json:"source_node_id"`
	SourceName   string `json:"source_name"`
	LastSync     int64  `json:"last_sync"`
}

type replicas struct {
	dir string // <节点目录>/replicas
	mu  sync.Mutex
	// 每个副本一把锁：同步写入、查看快照、恢复不能同时进行。
	locks map[string]*sync.Mutex
}

func replicaID(projectID, container string) (string, error) {
	if !projectIDRe.MatchString(projectID) {
		return "", status.Error(codes.InvalidArgument, "项目 ID 无效")
	}
	if err := spec.CheckContainerName(container); err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return projectID + "-" + container, nil
}

func (r *replicas) root(projectID, container string) (string, error) {
	id, err := replicaID(projectID, container)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.dir, id), nil
}

func (r *replicas) lock(projectID, container string) func() {
	id := projectID + "-" + container
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]*sync.Mutex{}
	}
	l := r.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// load 读取副本的登记信息和恢复用的配置。
func (r *replicas) load(projectID, container string) (*replicaMeta, *nagiv1.ContainerSpec, string, error) {
	root, err := r.root(projectID, container)
	if err != nil {
		return nil, nil, "", err
	}
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, root, status.Error(codes.NotFound, "本节点没有这个容器的副本")
	}
	if err != nil {
		return nil, nil, root, err
	}
	m := &replicaMeta{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, nil, root, err
	}
	s := &nagiv1.ContainerSpec{}
	raw, err := os.ReadFile(filepath.Join(root, "spec.pb"))
	if err != nil {
		return nil, nil, root, err
	}
	if err := proto.Unmarshal(raw, s); err != nil {
		return nil, nil, root, err
	}
	return m, s, root, nil
}

func (r *replicas) save(m *replicaMeta, s *nagiv1.ContainerSpec) error {
	root, err := r.root(m.ProjectID, m.Container)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "files", "repo"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeAtomic(filepath.Join(root, "meta.json"), b); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	raw, err := proto.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(root, "spec.pb"), raw)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// replicaStateDir 是副本快照仓库的连接配置和缓存，和容器自己的分开。
func (a *agent) replicaStateDir(projectID, container string) string {
	return filepath.Join(a.cfg.DataDir, "snapshots", "replica-"+projectID+"-"+container)
}

// ---- 源节点上的同步状态 ----

type backupEntry struct {
	Target    string `json:"target"` // 备份节点 ID
	LastSync  int64  `json:"last_sync"`
	LastError string `json:"last_error"`
	syncing   bool
}

// backupStates 记录每个容器同步到备份节点的结果，保存在 <节点目录>/backup-status.json。
type backupStates struct {
	file string
	mu   sync.Mutex
	m    map[string]*backupEntry // 项目 ID-容器名
}

func newBackupStates(file string) *backupStates {
	b := &backupStates{file: file, m: map[string]*backupEntry{}}
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &b.m)
	}
	return b
}

// entry 返回容器的状态；备份节点换了就从头记。调用时持有 mu。
func (b *backupStates) entry(id, target string) *backupEntry {
	e := b.m[id]
	if e == nil || e.Target != target {
		e = &backupEntry{Target: target}
		b.m[id] = e
	}
	return e
}

func (b *backupStates) persist() {
	raw, _ := json.Marshal(b.m)
	_ = writeAtomic(b.file, raw)
}

// status 返回容器的同步状态，没有设置备份节点时返回 nil。
func (b *backupStates) status(s *nagiv1.ContainerSpec) *nagiv1.BackupStatus {
	t := s.GetBackup()
	if t == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entry(s.GetProjectId()+"-"+s.GetName(), t.GetNodeId())
	out := &nagiv1.BackupStatus{TargetNodeId: t.GetNodeId(), TargetName: t.GetName(), LastError: e.LastError, Syncing: e.syncing}
	if e.LastSync > 0 {
		out.LastSync = timestamppb.New(time.Unix(e.LastSync, 0))
	}
	return out
}

// ---- 目标节点：MeshService ----

// authorizeReplica 校验调用方就是这个副本登记的源节点，返回副本的文件目录。
func (s *meshService) authorizeReplica(ctx context.Context, projectID, container string) (string, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	m, _, root, err := s.a.replicas.load(projectID, container)
	if err != nil {
		return "", err
	}
	if !slices.Equal(m.SourceKey, caller[:]) {
		return "", status.Error(codes.PermissionDenied, "这个容器的副本不接受来自你的同步")
	}
	return filepath.Join(root, "files"), nil
}

func (s *meshService) ReplicaManifest(ctx context.Context, req *nagiv1.ReplicaManifestRequest) (*nagiv1.ReplicaManifestResponse, error) {
	files, err := s.authorizeReplica(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ReplicaManifestResponse{}
	_ = filepath.WalkDir(files, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(files, full)
		out.Files = append(out.Files, &nagiv1.ReplicaFile{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return out, nil
}

func (s *meshService) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	var (
		files, path string
		f           *os.File
		tmp, dest   string
		count       uint32
		unlock      func()
	)
	finish := func() error {
		if f == nil {
			return nil
		}
		err := f.Close()
		f = nil
		if err == nil {
			err = os.Rename(tmp, dest)
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		count++
		return nil
	}
	defer func() {
		if f != nil {
			f.Close()
			os.Remove(tmp)
		}
		if unlock != nil {
			unlock()
		}
	}()
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			if err := finish(); err != nil {
				return err
			}
			return stream.SendAndClose(&nagiv1.ReplicaPutResponse{Files: count})
		}
		if err != nil {
			return err
		}
		if files == "" {
			if files, err = s.authorizeReplica(stream.Context(), c.GetProjectId(), c.GetContainer()); err != nil {
				return err
			}
			unlock = s.a.replicas.lock(c.GetProjectId(), c.GetContainer())
		}
		if c.GetPath() != path {
			if err := finish(); err != nil {
				return err
			}
			path = c.GetPath()
			if dest, err = project.ReplicaPath(files, path); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			tmp = dest + ".nagi-tmp"
			if f, err = os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil {
				return err
			}
		}
		if _, err := f.Write(c.GetData()); err != nil {
			return err
		}
	}
}

func (s *meshService) ReplicaCommit(ctx context.Context, req *nagiv1.ReplicaCommitRequest) (*nagiv1.ReplicaCommitResponse, error) {
	files, err := s.authorizeReplica(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	defer s.a.replicas.lock(req.GetProjectId(), req.GetContainer())()
	keep := map[string]bool{}
	for _, p := range req.GetPaths() {
		keep[p] = true
	}
	// 源节点上已经删掉的文件（例如快照仓库维护回收的数据包），这里也删掉。
	_ = filepath.WalkDir(files, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(files, full)
		if !keep[filepath.ToSlash(rel)] {
			os.Remove(full)
		}
		return nil
	})
	m, _, _, err := s.a.replicas.load(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	m.LastSync = time.Now().Unix()
	if err := s.a.replicas.save(m, nil); err != nil {
		return nil, err
	}
	return &nagiv1.ReplicaCommitResponse{}, nil
}

// ---- 源节点：同步 ----

// errAlreadySyncing：这个容器已经有一次同步在进行（同一个容器同时只有一次同步）。
var errAlreadySyncing = errors.New("已经在同步")

// runBackupSync 把容器同步到它的备份节点一次并记录结果；同步失败时告警，下一次成功时恢复。
// 没有设置备份节点时什么都不做；已经有一次同步在进行时返回 errAlreadySyncing。
func (a *agent) runBackupSync(ctx context.Context, projectID, container string) error {
	c, err := a.projects.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	t := c.Spec.GetBackup()
	if t == nil {
		return nil
	}
	id := projectID + "-" + container
	b := a.backups
	b.mu.Lock()
	e := b.entry(id, t.GetNodeId())
	if e.syncing {
		b.mu.Unlock()
		return errAlreadySyncing
	}
	e.syncing = true
	b.mu.Unlock()
	a.changed()

	_, err = a.syncTo(ctx, projectID, container, t)
	if status.Code(err) == codes.NotFound {
		err = fmt.Errorf("%s 上已经没有这个容器的副本登记（可能已经在那里恢复成了容器）。要继续备份，请重新选择一次备份节点（%w）", t.GetName(), err)
	}
	b.mu.Lock()
	e.syncing = false
	if err != nil {
		e.LastError = err.Error()
	} else {
		e.LastError, e.LastSync = "", time.Now().Unix()
	}
	b.persist()
	b.mu.Unlock()
	a.changed()

	key := "backup:" + id
	if err != nil {
		a.log.Warn("同步到备份节点失败", "project", projectID, "container", container, "err", err)
		a.alerts.Fire(ctx, key, "容器 "+container+" 同步到备份节点失败", err.Error(), projectID)
		return err
	}
	a.alerts.Resolve(ctx, key)
	return nil
}

// syncTo 把容器的快照仓库增量同步到目标节点，返回传输的字节数。备份和迁移共用。
func (a *agent) syncTo(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget) (int64, error) {
	if a.mesh == nil {
		return 0, errors.New("mesh 没有启动")
	}
	key, err := mesh.ParseKey(target.GetKey())
	if err != nil {
		return 0, err
	}
	cc, err := mesh.Client(a.mesh.m, key)
	if err != nil {
		return 0, err
	}
	defer cc.Close()
	c := nagiv1.NewMeshServiceClient(cc)
	man, err := c.ReplicaManifest(ctx, &nagiv1.ReplicaManifestRequest{ProjectId: projectID, Container: container})
	if err != nil {
		return 0, fmt.Errorf("连接节点 %s: %w", target.GetName(), err)
	}
	remote := map[string]int64{}
	for _, f := range man.GetFiles() {
		remote[f.GetPath()] = f.GetSize()
	}
	files, err := a.projects.ReplicaFiles(ctx, projectID, container)
	if err != nil {
		return 0, err
	}
	put, err := c.ReplicaPut(ctx)
	if err != nil {
		return 0, err
	}
	var paths []string
	var sent int64
	buf := make([]byte, 256<<10)
	first := true
	for _, f := range files {
		if size, ok := remote[f.Path]; ok && f.Immutable && size == f.Size {
			paths = append(paths, f.Path)
			continue
		}
		in, err := os.Open(f.Abs)
		if err != nil {
			continue // 维护时被删掉了，不再同步
		}
		start := true
		var sendErr error
		for {
			n, rerr := in.Read(buf)
			if n > 0 || start {
				chunk := &nagiv1.ReplicaChunk{Path: f.Path, Data: buf[:n]}
				if first {
					chunk.ProjectId, chunk.Container = projectID, container
					first = false
				}
				if sendErr = put.Send(chunk); sendErr != nil {
					break
				}
				sent += int64(n)
				start = false
			}
			if rerr != nil {
				break
			}
		}
		in.Close()
		if sendErr != nil {
			break // 真正的原因在 CloseAndRecv 里
		}
		paths = append(paths, f.Path)
	}
	if _, err := put.CloseAndRecv(); err != nil {
		return sent, fmt.Errorf("传输文件: %w", err)
	}
	if _, err := c.ReplicaCommit(ctx, &nagiv1.ReplicaCommitRequest{ProjectId: projectID, Container: container, Paths: paths}); err != nil {
		return sent, fmt.Errorf("提交: %w", err)
	}
	a.log.Info("已同步副本", "project", projectID, "container", container, "target", target.GetName(), "bytes", sent)
	return sent, nil
}

// ---- AgentService ----

// SyncBackup 立即同步一次，同步完才返回。已经有一次同步在进行时（例如快照之后自动开始的），
// 等它结束再做一次：那一次可能是在最新的快照之前开始的。
func (s *agentService) SyncBackup(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.BackupStatus, error) {
	c, err := s.projects.Get(ctx, req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, toStatus(err)
	}
	if c.Spec.GetBackup() == nil {
		return nil, status.Error(codes.FailedPrecondition, "这个容器没有设置备份节点")
	}
	err = s.node.runBackupSync(ctx, req.GetProjectId(), req.GetContainer())
	for errors.Is(err, errAlreadySyncing) {
		select {
		case <-ctx.Done():
			return nil, toStatus(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
		err = s.node.runBackupSync(ctx, req.GetProjectId(), req.GetContainer())
	}
	// 失败的原因记在状态里（last_error）。
	return s.node.backups.status(c.Spec), nil
}

func (s *agentService) AcceptReplica(_ context.Context, req *nagiv1.AcceptReplicaRequest) (*nagiv1.AcceptReplicaResponse, error) {
	if _, err := mesh.ParseKey(req.GetSourceKey()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	sp := req.GetSpec()
	r := s.node.replicas
	m, _, _, err := r.load(sp.GetProjectId(), sp.GetName())
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, err
		}
		m = &replicaMeta{ProjectID: sp.GetProjectId(), Container: sp.GetName()}
	}
	m.SourceKey, m.SourceNodeID, m.SourceName = req.GetSourceKey(), req.GetSourceNodeId(), req.GetSourceName()
	defer r.lock(sp.GetProjectId(), sp.GetName())()
	if err := r.save(m, sp); err != nil {
		return nil, err
	}
	return &nagiv1.AcceptReplicaResponse{}, nil
}

func (s *agentService) ListReplicas(context.Context, *nagiv1.ListReplicasRequest) (*nagiv1.ListReplicasResponse, error) {
	r := s.node.replicas
	entries, _ := os.ReadDir(r.dir)
	out := &nagiv1.ListReplicasResponse{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(r.dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		m := &replicaMeta{}
		if json.Unmarshal(raw, m) != nil {
			continue
		}
		_, sp, root, err := r.load(m.ProjectID, m.Container)
		if err != nil {
			continue
		}
		rp := &nagiv1.Replica{ProjectId: m.ProjectID, Container: m.Container, ProjectName: sp.GetProjectName(), SourceNodeId: m.SourceNodeID, SourceName: m.SourceName}
		if m.LastSync > 0 {
			rp.LastSync = timestamppb.New(time.Unix(m.LastSync, 0))
		}
		_ = filepath.WalkDir(filepath.Join(root, "files"), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					rp.Bytes += info.Size()
				}
			}
			return nil
		})
		out.Replicas = append(out.Replicas, rp)
	}
	return out, nil
}

func (s *agentService) ListReplicaSnapshots(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.ListSnapshotsResponse, error) {
	_, sp, root, err := s.node.replicas.load(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId(), req.GetContainer())()
	list, err := project.ReplicaSnapshots(ctx, sp, filepath.Join(root, "files", "repo"), s.node.replicaStateDir(req.GetProjectId(), req.GetContainer()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListSnapshotsResponse{Snapshots: list}, nil
}

func (s *agentService) CheckContainer(ctx context.Context, req *nagiv1.PutContainerRequest) (*nagiv1.CheckContainerResponse, error) {
	if err := s.projects.CheckPlacement(ctx, req.GetSpec()); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.CheckContainerResponse{}, nil
}

func (s *agentService) RestoreReplica(req *nagiv1.RestoreReplicaRequest, stream nagiv1.AgentService_RestoreReplicaServer) error {
	sp := req.GetSpec()
	if _, _, _, err := s.node.replicas.load(sp.GetProjectId(), sp.GetName()); err != nil {
		return err
	}
	defer s.changed()
	ctx := stream.Context()
	root, _ := s.node.replicas.root(sp.GetProjectId(), sp.GetName())
	unlock := s.node.replicas.lock(sp.GetProjectId(), sp.GetName())
	err := s.projects.RestoreReplica(ctx, sp, filepath.Join(root, "files", "repo"), req.GetSnapshotId(), func(line string) {
		_ = stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: line}})
	})
	unlock()
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrInvalidArgument) {
		return toStatus(err)
	}
	_, gerr := s.projects.Get(ctx, sp.GetProjectId(), sp.GetName())
	placed := gerr == nil
	if placed {
		// 容器已经用自己的快照仓库了，副本没用了。
		defer s.node.replicas.lock(sp.GetProjectId(), sp.GetName())()
		os.RemoveAll(root)
		os.RemoveAll(s.node.replicaStateDir(sp.GetProjectId(), sp.GetName()))
	}
	result := &nagiv1.DeployResult{Success: err == nil, Version: sp.GetVersion(), Placed: placed}
	if err != nil {
		result.Error = explain(err.Error())
	}
	return stream.Send(&nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Result{Result: result}})
}

func (s *agentService) DeleteReplica(_ context.Context, req *nagiv1.ContainerRef) (*nagiv1.DeleteReplicaResponse, error) {
	root, err := s.node.replicas.root(req.GetProjectId(), req.GetContainer())
	if err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId(), req.GetContainer())()
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	os.RemoveAll(s.node.replicaStateDir(req.GetProjectId(), req.GetContainer()))
	return &nagiv1.DeleteReplicaResponse{}, nil
}

// MigrateOut 是迁移的前半段：容器运行时先同步一次，停止并移除容器、保存最后一次快照，再增量同步一次。
func (s *agentService) MigrateOut(req *nagiv1.MigrateOutRequest, stream nagiv1.AgentService_MigrateOutServer) error {
	defer s.changed()
	t := req.GetTarget()
	if _, err := mesh.ParseKey(t.GetKey()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	ctx := stream.Context()
	pid, name := req.GetProjectId(), req.GetContainer()
	return runStream(stream.Send, func(onLine func(string)) error {
		onLine("第一次同步到 " + t.GetName() + "（容器照常运行）")
		n, err := s.node.syncTo(ctx, pid, name, t)
		if err != nil {
			return err
		}
		onLine("已同步 " + humanBytes(n))
		if err := s.projects.MigrateStop(ctx, pid, name, onLine); err != nil {
			return err
		}
		onLine("增量同步最后的数据")
		if n, err = s.node.syncTo(ctx, pid, name, t); err != nil {
			return err
		}
		onLine("已同步 " + humanBytes(n))
		return nil
	})
}

func (s *agentService) MigrateResume(req *nagiv1.ContainerRef, stream nagiv1.AgentService_MigrateResumeServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) error {
		return s.projects.MigrateResume(stream.Context(), req.GetProjectId(), req.GetContainer(), onLine)
	})
}
