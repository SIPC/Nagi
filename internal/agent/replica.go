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
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 备份节点（见 docs/snapshot.md#异地副本）。
//   - 源节点：每次定时快照成功后，把快照仓库和恢复所需的配置经 mesh 增量同步到备份节点。
//   - 备份节点：副本放在 <节点目录>/replicas/<项目 ID>/files，只接受登记过的源节点同步；
//     需要时把副本恢复成本节点上的项目。

var projectIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type replicaMeta struct {
	ProjectID    string `json:"project_id"`
	ProjectName  string `json:"project_name"`
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

func (r *replicas) root(id string) (string, error) {
	if !projectIDRe.MatchString(id) {
		return "", status.Error(codes.InvalidArgument, "项目 ID 无效")
	}
	return filepath.Join(r.dir, id), nil
}

func (r *replicas) lock(id string) func() {
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

func (r *replicas) load(id string) (*replicaMeta, string, error) {
	root, err := r.root(id)
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, root, status.Error(codes.NotFound, "本节点没有这个项目的副本")
	}
	if err != nil {
		return nil, root, err
	}
	m := &replicaMeta{}
	return m, root, json.Unmarshal(b, m)
}

func (r *replicas) save(m *replicaMeta) error {
	root, err := r.root(m.ProjectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := filepath.Join(root, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(root, "meta.json"))
}

// stateDir 是副本快照仓库的连接配置和缓存，和项目自己的分开。
func (a *agent) replicaStateDir(id string) string {
	return filepath.Join(a.cfg.DataDir, "snapshots", "replica-"+id)
}

// ---- 备份节点一侧：MeshService ----

// authorizeReplica 校验调用方就是这个副本登记的源节点。
func (s *meshService) authorizeReplica(ctx context.Context, id string) (string, error) {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	m, root, err := s.a.replicas.load(id)
	if err != nil {
		return "", err
	}
	if !slices.Equal(m.SourceKey, caller[:]) {
		return "", status.Error(codes.PermissionDenied, "这个项目的副本不接受来自你的同步")
	}
	return filepath.Join(root, "files"), nil
}

func (s *meshService) ReplicaManifest(ctx context.Context, req *nagiv1.ReplicaManifestRequest) (*nagiv1.ReplicaManifestResponse, error) {
	files, err := s.authorizeReplica(ctx, req.GetProjectId())
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
		id, files, path string
		f               *os.File
		tmp, dest       string
		count           uint32
		unlock          func()
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
		if id == "" {
			id = c.GetProjectId()
			if files, err = s.authorizeReplica(stream.Context(), id); err != nil {
				return err
			}
			unlock = s.a.replicas.lock(id)
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
	files, err := s.authorizeReplica(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	defer s.a.replicas.lock(req.GetProjectId())()
	// 项目目录的格式本节点不认识（源节点版本更新），不接收：以后也没法用它恢复。
	if raw, err := os.ReadFile(filepath.Join(files, "project.json")); err == nil {
		if err := project.CheckReplicaFormat(raw); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "备份节点不能接收这个项目："+err.Error())
		}
	}
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
	m, _, err := s.a.replicas.load(req.GetProjectId())
	if err != nil {
		return nil, err
	}
	if req.GetProjectName() != "" {
		m.ProjectName = req.GetProjectName()
	}
	m.LastSync = time.Now().Unix()
	if err := s.a.replicas.save(m); err != nil {
		return nil, err
	}
	// 热备：提前拉好镜像，顶替时只需要启动容器。
	if s.a.failovers.get(req.GetProjectId()) != nil {
		go s.a.prePull(context.WithoutCancel(ctx), req.GetProjectId())
	}
	return &nagiv1.ReplicaCommitResponse{}, nil
}

// ---- 源节点一侧：同步 ----

// errAlreadySyncing：这个项目已经有一次同步在进行（同一个项目同时只有一次同步）。
var errAlreadySyncing = errors.New("已经在同步")

// runBackupSync 同步一次并记录结果；同步失败时告警，下一次成功时恢复。
// 已经有一次同步在进行时直接返回 errAlreadySyncing。
func (a *agent) runBackupSync(ctx context.Context, id string) error {
	a.mu.Lock()
	if a.syncing == nil {
		a.syncing = map[string]bool{}
	}
	if a.syncing[id] {
		a.mu.Unlock()
		return errAlreadySyncing
	}
	a.syncing[id] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.syncing, id)
		a.mu.Unlock()
	}()

	err := a.syncBackup(ctx, id)
	if errors.Is(err, errNoBackupTarget) {
		return nil
	}
	a.projects.RecordBackupSync(ctx, id, err)
	if err != nil {
		a.log.Warn("同步到备份节点失败", "project", id, "err", err)
		a.alerts.Fire(ctx, "backup:"+id, "同步到备份节点失败", err.Error(), id)
		return err
	}
	a.alerts.Resolve(ctx, "backup:"+id)
	return nil
}

var errNoBackupTarget = errors.New("没有设置备份节点")

func (a *agent) syncBackup(ctx context.Context, id string) error {
	st, err := a.projects.BackupState(ctx, id)
	if err != nil {
		return err
	}
	if st.Target == nil {
		return errNoBackupTarget
	}
	_, err = a.syncTo(ctx, id, st.Target)
	return err
}

// syncTo 把项目的副本增量同步到目标节点，返回传输的字节数。备份和迁移共用。
func (a *agent) syncTo(ctx context.Context, id string, target *project.BackupTarget) (int64, error) {
	if a.mesh == nil {
		return 0, errors.New("mesh 没有启动")
	}
	key, err := mesh.ParseKey(target.Key)
	if err != nil {
		return 0, err
	}
	cc, err := mesh.Client(a.mesh.m, key)
	if err != nil {
		return 0, err
	}
	defer cc.Close()
	c := nagiv1.NewMeshServiceClient(cc)
	man, err := c.ReplicaManifest(ctx, &nagiv1.ReplicaManifestRequest{ProjectId: id})
	if err != nil {
		return 0, fmt.Errorf("连接节点 %s: %w", target.Name, err)
	}
	remote := map[string]int64{}
	for _, f := range man.GetFiles() {
		remote[f.GetPath()] = f.GetSize()
	}
	files, name, err := a.projects.ReplicaFiles(ctx, id)
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
	for _, f := range files {
		if size, ok := remote[f.Path]; ok && f.Immutable && size == f.Size {
			paths = append(paths, f.Path)
			continue
		}
		if f.Data != nil {
			if err := put.Send(&nagiv1.ReplicaChunk{ProjectId: id, Path: f.Path, Data: f.Data}); err != nil {
				break
			}
			paths = append(paths, f.Path)
			continue
		}
		in, err := os.Open(f.Abs)
		if err != nil {
			continue // 维护时被删掉了，不再同步
		}
		first := true
		var sendErr error
		for {
			n, rerr := in.Read(buf)
			if n > 0 || first {
				if sendErr = put.Send(&nagiv1.ReplicaChunk{ProjectId: id, Path: f.Path, Data: buf[:n]}); sendErr != nil {
					break
				}
				sent += int64(n)
				first = false
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
	if _, err := c.ReplicaCommit(ctx, &nagiv1.ReplicaCommitRequest{ProjectId: id, Paths: paths, ProjectName: name}); err != nil {
		return sent, fmt.Errorf("提交: %w", err)
	}
	a.log.Info("已同步副本", "project", id, "target", target.Name, "bytes", sent)
	return sent, nil
}

func (a *agent) backupStatus(ctx context.Context, id string) (*nagiv1.BackupStatus, error) {
	st, err := a.projects.BackupState(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &nagiv1.BackupStatus{LastError: st.LastError}
	if st.Target != nil {
		out.TargetNodeId, out.TargetName = st.Target.NodeID, st.Target.Name
	}
	if !st.LastSync.IsZero() {
		out.LastSync = timestamppb.New(st.LastSync)
	}
	a.mu.Lock()
	out.Syncing = a.syncing[id]
	a.mu.Unlock()
	return out, nil
}

// ---- AgentService：源节点一侧 ----

func (s *agentService) SetBackupTarget(ctx context.Context, req *nagiv1.SetBackupTargetRequest) (*nagiv1.BackupStatus, error) {
	var t *project.BackupTarget
	if len(req.GetTargetKey()) > 0 {
		if _, err := mesh.ParseKey(req.GetTargetKey()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		t = &project.BackupTarget{NodeID: req.GetTargetNodeId(), Name: req.GetTargetName(), Key: req.GetTargetKey()}
		// 顶替节点上已经有一份随时更新的副本；两套同步还会写同一个副本目录。
		if s.node.failovers.get(req.GetProjectId()) != nil {
			return nil, status.Error(codes.FailedPrecondition, "这个项目设了顶替，顶替节点上已经有一份随时更新的副本，不用再设备份节点")
		}
	}
	if err := s.projects.SetBackupTarget(ctx, req.GetProjectId(), t); err != nil {
		return nil, toStatus(err)
	}
	if t == nil {
		s.node.alerts.Resolve(ctx, "backup:"+req.GetProjectId())
	} else {
		go s.node.runBackupSync(context.WithoutCancel(ctx), req.GetProjectId())
	}
	return s.node.backupStatus(ctx, req.GetProjectId())
}

func (s *agentService) GetBackupStatus(ctx context.Context, req *nagiv1.GetBackupStatusRequest) (*nagiv1.BackupStatus, error) {
	return s.node.backupStatus(ctx, req.GetProjectId())
}

// SyncBackup 立即同步一次，同步完才返回。已经有一次同步在进行时（例如快照之后自动开始的），
// 等它结束再做一次：那一次可能是在最新的快照之前开始的。
func (s *agentService) SyncBackup(ctx context.Context, req *nagiv1.GetBackupStatusRequest) (*nagiv1.BackupStatus, error) {
	err := s.node.runBackupSync(ctx, req.GetProjectId())
	for errors.Is(err, errAlreadySyncing) {
		select {
		case <-ctx.Done():
			return nil, toStatus(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
		err = s.node.runBackupSync(ctx, req.GetProjectId())
	}
	if err != nil {
		st, _ := s.node.backupStatus(ctx, req.GetProjectId())
		if st != nil {
			return st, nil
		}
		return nil, toStatus(err)
	}
	return s.node.backupStatus(ctx, req.GetProjectId())
}

// ---- AgentService：备份节点一侧 ----

func (s *agentService) AcceptReplica(_ context.Context, req *nagiv1.AcceptReplicaRequest) (*nagiv1.AcceptReplicaResponse, error) {
	if _, err := mesh.ParseKey(req.GetSourceKey()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	r := s.node.replicas
	m, _, err := r.load(req.GetProjectId())
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, err
		}
		m = &replicaMeta{ProjectID: req.GetProjectId()}
	}
	m.ProjectName, m.SourceKey = req.GetProjectName(), req.GetSourceKey()
	m.SourceNodeID, m.SourceName = req.GetSourceNodeId(), req.GetSourceName()
	if err := r.save(m); err != nil {
		return nil, err
	}
	return &nagiv1.AcceptReplicaResponse{}, nil
}

func (s *agentService) ListReplicas(context.Context, *nagiv1.ListReplicasRequest) (*nagiv1.ListReplicasResponse, error) {
	r := s.node.replicas
	entries, _ := os.ReadDir(r.dir)
	out := &nagiv1.ListReplicasResponse{}
	for _, e := range entries {
		m, root, err := r.load(e.Name())
		if err != nil {
			continue
		}
		rp := &nagiv1.Replica{ProjectId: m.ProjectID, ProjectName: m.ProjectName, SourceNodeId: m.SourceNodeID, SourceName: m.SourceName}
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

func (s *agentService) ListReplicaSnapshots(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.ListSnapshotsResponse, error) {
	_, root, err := s.node.replicas.load(req.GetProjectId())
	if err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId())()
	list, err := project.ReplicaSnapshots(ctx, req.GetProjectId(), filepath.Join(root, "files"), s.node.replicaStateDir(req.GetProjectId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.ListSnapshotsResponse{Snapshots: list}, nil
}

func (s *agentService) RestoreReplica(req *nagiv1.RestoreReplicaRequest, stream nagiv1.AgentService_RestoreReplicaServer) error {
	if _, _, err := s.node.replicas.load(req.GetProjectId()); err != nil {
		return err
	}
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		_, err := s.node.restoreReplica(stream.Context(), req.GetProjectId(), req.GetDir(), req.GetSnapshotId(), onLine)
		return 0, err
	})
}

func (s *agentService) DeleteReplica(_ context.Context, req *nagiv1.ReplicaRef) (*nagiv1.DeleteReplicaResponse, error) {
	root, err := s.node.replicas.root(req.GetProjectId())
	if err != nil {
		return nil, err
	}
	defer s.node.replicas.lock(req.GetProjectId())()
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	os.RemoveAll(s.node.replicaStateDir(req.GetProjectId()))
	return &nagiv1.DeleteReplicaResponse{}, nil
}

func (s *agentService) CheckRestore(ctx context.Context, req *nagiv1.CheckRestoreRequest) (*nagiv1.CheckRestoreResponse, error) {
	if err := s.projects.CheckRestore(ctx, req.GetProjectId(), req.GetDir(), req.GetRoutes()); err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.CheckRestoreResponse{}, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- 手动迁移：源节点一侧 ----

func (s *agentService) MigrateOut(req *nagiv1.MigrateOutRequest, stream nagiv1.AgentService_MigrateOutServer) error {
	defer s.changed()
	id := req.GetProjectId()
	if _, err := mesh.ParseKey(req.GetTargetKey()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	target := &project.BackupTarget{NodeID: req.GetTargetNodeId(), Name: req.GetTargetName(), Key: req.GetTargetKey()}
	ctx := stream.Context()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		onLine("第一次同步到 " + target.Name + "（项目照常运行）")
		n, err := s.node.syncTo(ctx, id, target)
		if err != nil {
			return 0, err
		}
		onLine("已同步 " + humanBytes(n))
		if _, err := s.projects.MigrateStop(ctx, id, onLine); err != nil {
			return 0, err
		}
		onLine("增量同步最后的数据")
		if n, err = s.node.syncTo(ctx, id, target); err != nil {
			return 0, err
		}
		onLine("已同步 " + humanBytes(n))
		return 0, nil
	})
}

func (s *agentService) MigrateResume(req *nagiv1.ReplicaRef, stream nagiv1.AgentService_MigrateResumeServer) error {
	defer s.changed()
	return runStream(stream.Send, func(onLine func(string)) (int64, error) {
		return 0, s.projects.MigrateResume(stream.Context(), req.GetProjectId(), onLine)
	})
}

func (s *agentService) MigrateFinish(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.MigrateFinishResponse, error) {
	defer s.changed()
	dir, err := s.projects.MigrateFinish(ctx, req.GetProjectId())
	if err != nil {
		return nil, toStatus(err)
	}
	s.node.log.Info("项目已迁移到其他节点，目录保留", "project", req.GetProjectId(), "dir", dir)
	return &nagiv1.MigrateFinishResponse{Dir: dir}, nil
}
