package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"github.com/SIPC/nagi/internal/agent/project"
	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
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
	locks map[string]*sync.RWMutex
}

func replicaID(projectID, container string) (string, error) {
	if !projectIDRe.MatchString(projectID) {
		return "", status.Error(codes.InvalidArgument, "项目 ID 格式不正确")
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
	l := r.lockOf(projectID, container)
	l.Lock()
	return l.Unlock
}

// rlock 是接收文件时加的锁：同一次同步的几条流（见 syncStreams）各写各的文件，可以同时拿；
// 提交、恢复等要改整个副本的操作用 lock，等它们都结束。
func (r *replicas) rlock(projectID, container string) func() {
	l := r.lockOf(projectID, container)
	l.RLock()
	return l.RUnlock
}

func (r *replicas) lockOf(projectID, container string) *sync.RWMutex {
	id := projectID + "-" + container
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks == nil {
		r.locks = map[string]*sync.RWMutex{}
	}
	l := r.locks[id]
	if l == nil {
		l = &sync.RWMutex{}
		r.locks[id] = l
	}
	return l
}

// load 读取副本的登记信息和恢复用的配置。
func (r *replicas) load(projectID, container string) (*replicaMeta, *nagiv1.ContainerSpec, string, error) {
	root, err := r.root(projectID, container)
	if err != nil {
		return nil, nil, "", err
	}
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, root, status.Error(codes.NotFound, "此节点上没有该容器的副本")
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
		s.a.violations.add(caller, "replica:sync", projectID, container, 0, "not the replica's source node")
		return "", status.Error(codes.PermissionDenied, "该容器的副本不接受来自此节点的同步")
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
		if err != nil || isReplicaTemp(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(files, full)
		out.Files = append(out.Files, &nagiv1.ReplicaFile{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return out, nil
}

// replicaTempMark 是接收中的临时文件名里的标记：<文件名>.nagi-tmp-<随机>。随机后缀让同时写同一个文件的两条流
// 各写各的临时文件，不会互相覆盖；清单不列它们，提交时作为多余的文件删掉。
const replicaTempMark = ".nagi-tmp"

func isReplicaTemp(name string) bool { return strings.Contains(name, replicaTempMark) }

// replicaIdle 是同步副本时多久收不到数据就放弃：源节点挂了、网络断了时不一直占着锁和临时文件。
const replicaIdle = 2 * time.Minute

// ReplicaPut 接收源节点同步来的文件。不限大小（快照仓库可以很大），但磁盘剩余空间不够时停下（见 metrics.CheckDisk）。
func (s *meshService) ReplicaPut(stream nagiv1.MeshService_ReplicaPutServer) error {
	var (
		files, path string
		f           *os.File
		tmp, dest   string
		count       uint32
		unlock      func()
		guard       *metrics.DiskGuard
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
	// 单独的 goroutine 收数据，这里等的时候能超时。返回以后流结束，Recv 随之出错退出。
	type recv struct {
		c   *nagiv1.ReplicaChunk
		err error
	}
	// 带缓冲：这边写盘的时候那边接着收，写盘慢一点也不会马上让发送方停下来。
	ch := make(chan recv, 16)
	go func() {
		for {
			c, err := stream.Recv()
			select {
			case ch <- recv{c, err}:
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	idle := time.NewTimer(replicaIdle)
	defer idle.Stop()
	for {
		var r recv
		select {
		case r = <-ch:
		case <-idle.C:
			return status.Error(codes.DeadlineExceeded, "同步副本时长时间没有收到数据")
		}
		idle.Reset(replicaIdle)
		c, err := r.c, r.err
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
			unlock = s.a.replicas.rlock(c.GetProjectId(), c.GetContainer())
			guard = &metrics.DiskGuard{Dir: files}
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
			tmp = dest + replicaTempMark + "-" + randomSuffix()
			if f, err = os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil {
				return err
			}
		}
		if err := guard.Check(len(c.GetData())); err != nil {
			return status.Error(codes.ResourceExhausted, err.Error())
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
	// 热备：提前拉好镜像，顶替时只需要启动容器。
	if s.a.managed(req.GetProjectId(), req.GetContainer()) {
		go s.a.prePull(context.WithoutCancel(ctx), req.GetProjectId(), req.GetContainer())
		s.a.changed()
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
		if a.alerts.ContainerEnabled(projectID, container, alert.RuleBackup) {
			a.alerts.Fire(ctx, key, "容器 "+container+" 同步到备份节点失败", err.Error(), projectID)
		}
		return err
	}
	a.alerts.Resolve(ctx, key)
	return nil
}

// syncTo 把容器的快照仓库增量同步到目标节点，返回传输的字节数。备份和迁移共用。
// 连接中途断了（大文件走慢线路时连接超时，或者对方节点刚重启，旧的 QUIC 连接收到无状态重置）时换一条新连接接着传：
// 对方每收完一个文件就落盘，下次核对清单时跳过，所以每次重试都从断开的地方往后。上一次传过去了数据就继续重试，
// 最多 syncAttempts 次；一点都没传过去就不再试（线路是真的不通）。
func (a *agent) syncTo(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget) (int64, error) {
	// 同一个容器到同一个目标同时只有一次同步：定时快照后的同步、每 15 分钟一次的热备同步、迁移、切回可能重叠，
	// 两次同步交错时，一次提交会删掉另一次刚传的文件。后来的等前一次结束。
	unlock, err := lockSync(ctx, projectID+"/"+container+"/"+string(target.GetKey()))
	if err != nil {
		return 0, err
	}
	defer unlock()
	return resumeSync(ctx, func() (int64, error) { return a.syncOnce(ctx, projectID, container, target) }, func(attempt int, n int64, err error) {
		a.log.Info("同步时连接断开，接着传", "project", projectID, "container", container, "target", target.GetName(),
			"attempt", attempt, "bytes", n, "err", err)
	})
}

// resumeSync 按 syncTo 说的规则反复调用 once，返回一共传了多少字节和最后一次的结果。
func resumeSync(ctx context.Context, once func() (int64, error), onRetry func(attempt int, n int64, err error)) (int64, error) {
	var total int64
	for attempt := 1; ; attempt++ {
		n, err := once()
		total += n
		if err == nil || !connectionLost(err) || attempt == syncAttempts || (attempt > 1 && n == 0) {
			return total, err
		}
		onRetry(attempt, n, err)
		select {
		case <-ctx.Done():
			return total, err
		case <-time.After(syncRetryDelay):
		}
	}
}

// syncAttempts 是一次同步最多连接几次；syncRetryDelay 是断开以后等多久再连。
const syncAttempts = 10

var syncRetryDelay = 2 * time.Second

// connectionLost 判断同步失败是不是因为传输断了，而不是对方拒绝：连接断开（Unavailable），
// 或者对方太久没收到数据（DeadlineExceeded，例如旧版本的对方一次只收一条流，其余的流等它时超时）。
func connectionLost(err error) bool {
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return false
	}
	c := se.GRPCStatus().Code()
	return c == codes.Unavailable || c == codes.DeadlineExceeded
}

// syncLocks 是正在进行的同步，键是容器和目标，值是只能放一个的通道（能被 ctx 取消的锁）。
var syncLocks sync.Map

// lockSync 等到 key 没有别的同步在进行，返回释放的函数。
func lockSync(ctx context.Context, key string) (func(), error) {
	v, _ := syncLocks.LoadOrStore(key, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// randomSuffix 是临时文件名的随机后缀。
func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (a *agent) syncOnce(ctx context.Context, projectID, container string, target *nagiv1.BackupTarget) (int64, error) {
	// 同步期间仓库的文件不能被立即清理删掉，否则备份节点会收到不完整的一套文件。
	defer a.projects.ReadRepo(projectID, container)()
	if a.mesh == nil {
		return 0, errors.New("节点网络没有启动")
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
	// 对方已经有、而且不会再变的文件不用传，直接算进提交的清单。数据包先传，都到了再传引用它们的索引等文件：
	// 中途断开时，备份节点上不会出现指向还没收到的数据包的索引。
	var paths []string
	var packs, others []project.ReplicaFile
	for _, f := range files {
		switch size, ok := remote[f.Path]; {
		case ok && f.Immutable && size == f.Size:
			paths = append(paths, f.Path)
		case f.Pack:
			packs = append(packs, f)
		default:
			others = append(others, f)
		}
	}
	var sent atomic.Int64
	for _, batch := range [][]project.ReplicaFile{packs, others} {
		dial := func() (*grpc.ClientConn, error) { return mesh.Client(a.mesh.m, key) }
		done, err := sendReplicaFiles(ctx, dial, cc, projectID, container, batch, &sent)
		paths = append(paths, done...)
		if err != nil {
			return sent.Load(), err
		}
	}
	if _, err := c.ReplicaCommit(ctx, &nagiv1.ReplicaCommitRequest{ProjectId: projectID, Container: container, Paths: paths}); err != nil {
		return sent.Load(), fmt.Errorf("提交: %w", err)
	}
	a.log.Info("已同步副本", "project", projectID, "container", container, "target", target.GetName(), "bytes", sent.Load())
	return sent.Load(), nil
}

// syncStreams 是同步副本时最多同时开几条流（每条是单独的一个 mesh 连接，走 QUIC 时是同一条连接上的不同流）。
// 文件多、线路延迟高时，几个文件一起传比一个接一个快；对方写盘也能并行。
const syncStreams = 4

// sendReplicaFiles 用最多 syncStreams 条 ReplicaPut 流把 files 传给对方，各条流从同一个队列里取文件。
// 返回传完了的文件（对方收到并确认了）。first 是已经建好的连接，第一条流用它，其余的各用 dial 建一个。
func sendReplicaFiles(ctx context.Context, dial func() (*grpc.ClientConn, error), first *grpc.ClientConn, projectID, container string,
	files []project.ReplicaFile, sent *atomic.Int64) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	queue := make(chan project.ReplicaFile, len(files))
	for _, f := range files {
		queue <- f
	}
	close(queue)
	var mu sync.Mutex
	var done []string
	g, gctx := errgroup.WithContext(ctx)
	for i := range min(syncStreams, len(files)) {
		g.Go(func() error {
			cc := first
			if i > 0 {
				var err error
				if cc, err = dial(); err != nil {
					return err
				}
				defer cc.Close()
			}
			ok, err := sendReplicaStream(gctx, nagiv1.NewMeshServiceClient(cc), projectID, container, queue, sent)
			if err != nil {
				return err
			}
			mu.Lock()
			done = append(done, ok...)
			mu.Unlock()
			return nil
		})
	}
	err := g.Wait()
	return done, err
}

// sendReplicaStream 在一条 ReplicaPut 流上传队列里的文件，直到队列空了，返回这条流上传完的文件。
func sendReplicaStream(ctx context.Context, c nagiv1.MeshServiceClient, projectID, container string,
	queue <-chan project.ReplicaFile, sent *atomic.Int64) ([]string, error) {
	var put nagiv1.MeshService_ReplicaPutClient
	var paths []string
	buf := make([]byte, 256<<10)
	for f := range queue {
		in, err := os.Open(f.Abs)
		if err != nil {
			continue // 维护时被删掉了，不再同步
		}
		if put == nil {
			if put, err = c.ReplicaPut(ctx); err != nil {
				in.Close()
				return nil, err
			}
		}
		start := true
		var sendErr error
		for {
			n, rerr := in.Read(buf)
			if n > 0 || start {
				chunk := &nagiv1.ReplicaChunk{Path: f.Path, Data: buf[:n]}
				// 每条流的第一块带上容器，对方据此核对权限、加锁。
				if start && len(paths) == 0 {
					chunk.ProjectId, chunk.Container = projectID, container
				}
				if sendErr = put.Send(chunk); sendErr != nil {
					break
				}
				sent.Add(int64(n))
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
	if put == nil {
		return nil, nil
	}
	if _, err := put.CloseAndRecv(); err != nil {
		return nil, fmt.Errorf("传输文件: %w", err)
	}
	return paths, nil
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
		return nil, status.Error(codes.FailedPrecondition, "该容器没有设置备份节点")
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
